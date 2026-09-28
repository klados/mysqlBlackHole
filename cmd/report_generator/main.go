package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// aggSize caps how many buckets the terms aggregations return. It matches
// Elasticsearch's default search.max_buckets budget and is small enough to
// render quickly. When a report hits the cap it says so instead of silently
// dropping the tail.
const aggSize = 10000

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// wrappedError annotates an error with context while preserving the chain
// for errors.Is and errors.As.
type wrappedError struct {
	msg string
	err error
}

func (e *wrappedError) Error() string { return e.msg + ": " + e.err.Error() }

func (e *wrappedError) Unwrap() error { return e.err }

func wrapErr(err error, msg string) error {
	return &wrappedError{msg: msg, err: err}
}

// truncate shortens s to at most n bytes for embedding response bodies in
// errors.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ESClient is a minimal stdlib Elasticsearch client (no external deps).
// It talks to the same cluster Vector ships logs to (see vector.yaml).
type ESClient struct {
	baseURL string
	index   string
	http    *http.Client
	user    string
	pass    string
}

func newESClient() *ESClient {
	return &ESClient{
		baseURL: strings.TrimSuffix(getEnv("ELASTICSEARCH_URL", "http://localhost:9200"), "/"),
		index:   getEnv("ES_INDEX", "mysqlblackhole"),
		http:    &http.Client{Timeout: 10 * time.Second},
		user:    os.Getenv("ELASTICSEARCH_USER"),
		pass:    os.Getenv("ELASTICSEARCH_PASSWORD"),
	}
}

func (c *ESClient) do(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, wrapErr(err, "build request for "+path)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, wrapErr(err, method+" "+path)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, wrapErr(err, "read response for "+path)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(method + " " + path + ": unexpected status " + resp.Status + ": " + truncate(string(b), 500))
	}
	return b, nil
}

func (c *ESClient) doGet(ctx context.Context, path string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

func (c *ESClient) doPost(ctx context.Context, path string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, wrapErr(err, "encode request for "+path)
	}
	return c.do(ctx, http.MethodPost, path, raw)
}

func isIndexNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "index_not_found_exception") ||
		strings.Contains(msg, "404 Not Found")
}

func (c *ESClient) ping(ctx context.Context) (string, string, error) {
	body, err := c.doGet(ctx, "/")
	if err != nil {
		return "", "", err
	}
	var decoded struct {
		ClusterName string `json:"cluster_name"`
		Version     struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", "", wrapErr(err, "decode root info")
	}
	return decoded.ClusterName, decoded.Version.Number, nil
}

func (c *ESClient) clusterHealth(ctx context.Context) (string, error) {
	body, err := c.doGet(ctx, "/_cluster/health")
	if err != nil {
		return "", err
	}
	var decoded struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", wrapErr(err, "decode cluster health")
	}
	return decoded.Status, nil
}

// fields holds the concrete Elasticsearch field names resolved from the index
// mapping. A string field may be mapped as keyword or as text with a .keyword
// subfield; aggregations and exact filters need the keyword form, date ranges
// need the date form.
type fields struct {
	Time string
	User string
	IP   string
	Msg  string
}

// resolveFields discovers, in a single field-caps call, which field names are
// usable for the reports. A missing index is reported as an error the caller
// can recognise with isIndexNotFound.
func (c *ESClient) resolveFields(ctx context.Context) (fields, error) {
	all := []string{
		"time", "time.keyword",
		"@timestamp", "@timestamp.keyword",
		"timestamp", "timestamp.keyword",
		"user", "user.keyword",
		"ip", "ip.keyword",
		"msg", "msg.keyword",
	}
	body, err := c.doGet(ctx, "/"+c.index+"/_field_caps?fields="+strings.Join(all, ","))
	if err != nil {
		return fields{}, err
	}
	caps, err := parseCaps(body)
	if err != nil {
		return fields{}, err
	}

	f := fields{
		Time: pickField(caps, "date", "time", "@timestamp", "timestamp"),
		User: pickField(caps, "keyword", "user"),
		IP:   pickField(caps, "keyword", "ip"),
		Msg:  pickField(caps, "keyword", "msg"),
	}
	if f.Time == "" {
		return f, errors.New("no date field found in index " + strconv.Quote(c.index) + " (looked for time, @timestamp, timestamp)")
	}
	return f, nil
}

func parseCaps(body []byte) (map[string]map[string]json.RawMessage, error) {
	var resp struct {
		Fields map[string]map[string]json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, wrapErr(err, "decode field caps")
	}
	return resp.Fields, nil
}

// pickField returns the first candidate usable for allowedType, preferring the
// exact name and falling back to its .keyword subfield.
func pickField(caps map[string]map[string]json.RawMessage, allowedType string, candidates ...string) string {
	for _, cand := range candidates {
		for _, name := range []string{cand, cand + ".keyword"} {
			if types, ok := caps[name]; ok {
				if _, ok := types[allowedType]; ok {
					return name
				}
			}
		}
	}
	return ""
}

func rangeQuery(field string, start, end time.Time) map[string]any {
	return map[string]any{
		"range": map[string]any{
			field: map[string]any{
				"gte": start.UTC().Format(time.RFC3339Nano),
				"lte": end.UTC().Format(time.RFC3339Nano),
			},
		},
	}
}

// countInWindow returns how many documents fall inside the window.
func (c *ESClient) countInWindow(ctx context.Context, f fields, start, end time.Time) (int64, error) {
	body, err := c.doPost(ctx, "/"+c.index+"/_search", map[string]any{
		"size":             0,
		"track_total_hits": true,
		"query":            rangeQuery(f.Time, start, end),
	})
	if err != nil {
		return 0, err
	}
	return parseTotalHits(body)
}

type userStat struct {
	User     string
	Attempts int64
	Success  int64
	Failure  int64
}

type ipStat struct {
	IP     string
	Events int64
}

type bucket struct {
	Key      string `json:"key"`
	DocCount int64  `json:"doc_count"`
	Success  struct {
		DocCount int64 `json:"doc_count"`
	} `json:"success"`
	Failure struct {
		DocCount int64 `json:"doc_count"`
	} `json:"failure"`
}

type termsAgg struct {
	DocCountErrorUpperBound int64    `json:"doc_count_error_upper_bound"`
	SumOtherDocCount        int64    `json:"sum_other_doc_count"`
	Buckets                 []bucket `json:"buckets"`
}

func parseTotalHits(body []byte) (int64, error) {
	var resp struct {
		Hits struct {
			Total json.RawMessage `json:"total"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, wrapErr(err, "decode search hits")
	}
	raw := bytes.TrimSpace(resp.Hits.Total)
	if len(raw) > 0 && raw[0] == '{' {
		var obj struct {
			Value int64 `json:"value"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return 0, wrapErr(err, "decode total hits")
		}
		return obj.Value, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, wrapErr(err, "decode total hits")
	}
	return n, nil
}

func parseUserAgg(body []byte) ([]userStat, bool, error) {
	var resp struct {
		Aggregations struct {
			Users termsAgg `json:"users"`
		} `json:"aggregations"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false, wrapErr(err, "decode username aggregation")
	}
	agg := resp.Aggregations.Users
	stats := make([]userStat, 0, len(agg.Buckets))
	for _, b := range agg.Buckets {
		stats = append(stats, userStat{
			User:     b.Key,
			Attempts: b.DocCount,
			Success:  b.Success.DocCount,
			Failure:  b.Failure.DocCount,
		})
	}
	return stats, agg.SumOtherDocCount > 0, nil
}

func parseIPAgg(body []byte) ([]ipStat, bool, error) {
	var resp struct {
		Aggregations struct {
			IPs termsAgg `json:"ips"`
		} `json:"aggregations"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, false, wrapErr(err, "decode ip aggregation")
	}
	agg := resp.Aggregations.IPs
	stats := make([]ipStat, 0, len(agg.Buckets))
	for _, b := range agg.Buckets {
		stats = append(stats, ipStat{IP: b.Key, Events: b.DocCount})
	}
	return stats, agg.SumOtherDocCount > 0, nil
}

// usersTried counts authentication attempts per username. Only auth_success and
// auth_failure events count, so a busy session does not inflate the total.
func (c *ESClient) usersTried(ctx context.Context, f fields, start, end time.Time) ([]userStat, bool, error) {
	if f.User == "" || f.Msg == "" {
		return nil, false, nil
	}
	body, err := c.doPost(ctx, "/"+c.index+"/_search", map[string]any{
		"size": 0,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					rangeQuery(f.Time, start, end),
					map[string]any{"terms": map[string]any{f.Msg: []string{"auth_success", "auth_failure"}}},
				},
			},
		},
		"aggs": map[string]any{
			"users": map[string]any{
				"terms": map[string]any{"field": f.User, "size": aggSize, "order": map[string]any{"_count": "desc"}},
				"aggs": map[string]any{
					"success": map[string]any{"filter": map[string]any{"term": map[string]any{f.Msg: "auth_success"}}},
					"failure": map[string]any{"filter": map[string]any{"term": map[string]any{f.Msg: "auth_failure"}}},
				},
			},
		},
	})
	if err != nil {
		return nil, false, err
	}
	return parseUserAgg(body)
}

// sourceIPs counts every honeypot log line per source IP in the window, not just
// authentication events.
func (c *ESClient) sourceIPs(ctx context.Context, f fields, start, end time.Time) ([]ipStat, bool, error) {
	if f.IP == "" {
		return nil, false, nil
	}
	body, err := c.doPost(ctx, "/"+c.index+"/_search", map[string]any{
		"size": 0,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []any{
					rangeQuery(f.Time, start, end),
					map[string]any{"exists": map[string]any{"field": f.IP}},
				},
			},
		},
		"aggs": map[string]any{
			"ips": map[string]any{
				"terms": map[string]any{"field": f.IP, "size": aggSize, "order": map[string]any{"_count": "desc"}},
			},
		},
	})
	if err != nil {
		return nil, false, err
	}
	return parseIPAgg(body)
}

type report struct {
	Index          string
	Window         time.Duration
	Start          time.Time
	End            time.Time
	Docs           int64
	Users          []userStat
	UsersTruncated bool
	IPs            []ipStat
	IPsTruncated   bool
}

// humanDuration renders a duration for a heading: 24h -> "24h", 90m -> "90m".
func humanDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return d.String()
	}
}

// mdEscape keeps attacker-controlled usernames and IPs from breaking the
// Markdown table layout.
func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func renderMarkdown(r report) string {
	var b strings.Builder
	b.WriteString("# mysqlBlackHole report\n\n")
	b.WriteString("Window: last " + humanDuration(r.Window) + " (" +
		r.Start.UTC().Format(time.RFC3339) + " .. " +
		r.End.UTC().Format(time.RFC3339) + ")\n")
	b.WriteString("Index: `" + r.Index + "`\n")
	b.WriteString("Docs in window: " + strconv.FormatInt(r.Docs, 10) + "\n\n")

	b.WriteString("## Usernames tried\n\n")
	if len(r.Users) == 0 {
		b.WriteString("_No authentication attempts in this window._\n\n")
	} else {
		b.WriteString("| user | attempts | success | failure |\n")
		b.WriteString("|------|----------|---------|---------|\n")
		for _, u := range r.Users {
			b.WriteString("| " + mdEscape(u.User) +
				" | " + strconv.FormatInt(u.Attempts, 10) +
				" | " + strconv.FormatInt(u.Success, 10) +
				" | " + strconv.FormatInt(u.Failure, 10) + " |\n")
		}
		if r.UsersTruncated {
			b.WriteString("\n_More than " + strconv.Itoa(aggSize) + " usernames; showing the top " + strconv.Itoa(aggSize) + "._\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## Source IPs\n\n")
	if len(r.IPs) == 0 {
		b.WriteString("_No source IPs in this window._\n")
	} else {
		b.WriteString("| ip | events |\n")
		b.WriteString("|----|--------|\n")
		for _, s := range r.IPs {
			b.WriteString("| " + mdEscape(s.IP) + " | " + strconv.FormatInt(s.Events, 10) + " |\n")
		}
		if r.IPsTruncated {
			b.WriteString("\n_More than " + strconv.Itoa(aggSize) + " source IPs; showing the top " + strconv.Itoa(aggSize) + "._\n")
		}
	}

	return b.String()
}

func collect(ctx context.Context, c *ESClient, window time.Duration) (report, error) {
	end := time.Now().UTC()
	rep := report{
		Index:  c.index,
		Window: window,
		Start:  end.Add(-window),
		End:    end,
	}

	f, err := c.resolveFields(ctx)
	if err != nil {
		if isIndexNotFound(err) {
			slog.Warn("index does not exist yet, no logs shipped", slog.String("index", c.index))
			return rep, nil
		}
		return rep, wrapErr(err, "resolve index fields")
	}

	docs, err := c.countInWindow(ctx, f, rep.Start, rep.End)
	if err != nil {
		return rep, wrapErr(err, "count docs in window")
	}
	rep.Docs = docs

	users, usersTruncated, err := c.usersTried(ctx, f, rep.Start, rep.End)
	if err != nil {
		return rep, wrapErr(err, "aggregate usernames")
	}
	rep.Users, rep.UsersTruncated = users, usersTruncated

	ips, ipsTruncated, err := c.sourceIPs(ctx, f, rep.Start, rep.End)
	if err != nil {
		return rep, wrapErr(err, "aggregate source ips")
	}
	rep.IPs, rep.IPsTruncated = ips, ipsTruncated

	return rep, nil
}

var webhookClient = &http.Client{Timeout: 20 * time.Second}

// sendToDiscord posts the Markdown report as a file, with a one-line summary.
// The webhook URL is a secret and is never included in errors or logs.
func sendToDiscord(ctx context.Context, webhookURL, content string, markdown []byte) error {
	if webhookURL == "" {
		return errors.New("DISCORD_REPORT_WEBHOOK is not set")
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	payload, err := json.Marshal(map[string]any{"content": content})
	if err != nil {
		return wrapErr(err, "encode webhook payload")
	}
	if err := w.WriteField("payload_json", string(payload)); err != nil {
		return wrapErr(err, "write webhook payload")
	}
	fw, err := w.CreateFormFile("files[0]", "report.md")
	if err != nil {
		return wrapErr(err, "create webhook attachment")
	}
	if _, err := fw.Write(markdown); err != nil {
		return wrapErr(err, "write webhook attachment")
	}
	if err := w.Close(); err != nil {
		return wrapErr(err, "finalize webhook body")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, &buf)
	if err != nil {
		return wrapErr(err, "build webhook request")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := webhookClient.Do(req)
	if err != nil {
		return wrapErr(err, "post report to webhook")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return errors.New("webhook returned " + resp.Status + ": " + truncate(string(body), 500))
	}
	return nil
}

func run(ctx context.Context, window time.Duration) error {
	if window <= 0 {
		return errors.New("window must be positive, got " + window.String())
	}

	client := newESClient()

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cluster, version, err := client.ping(ctx)
	if err != nil {
		return wrapErr(err, "connect to elasticsearch at "+client.baseURL)
	}
	slog.Info("connected to elasticsearch",
		slog.String("url", client.baseURL),
		slog.String("cluster", cluster),
		slog.String("version", version),
	)

	health, err := client.clusterHealth(ctx)
	if err != nil {
		return wrapErr(err, "get cluster health")
	}

	rep, err := collect(ctx, client, window)
	if err != nil {
		return err
	}

	slog.Info("report summary",
		slog.String("index", client.index),
		slog.String("health", health),
		slog.String("window", window.String()),
		slog.Int64("docs", rep.Docs),
		slog.Int("users", len(rep.Users)),
		slog.Int("ips", len(rep.IPs)),
	)

	md := renderMarkdown(rep)
	if _, err := io.WriteString(os.Stdout, md); err != nil {
		return wrapErr(err, "write report to stdout")
	}

	content := "**mysqlBlackHole report** — last " + humanDuration(window) +
		" (" + strconv.FormatInt(rep.Docs, 10) + " docs in window)"
	if err := sendToDiscord(ctx, os.Getenv("DISCORD_REPORT_WEBHOOK"), content, []byte(md)); err != nil {
		return err
	}
	slog.Info("report delivered")

	return nil
}

func main() {
	// Logs go to stderr so the Markdown report on stdout stays pipeable.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)).With(slog.String("service", "report-generator")))

	window := flag.Duration("window", 24*time.Hour, "lookback window for the report (e.g. 30m, 1h, 24h, 168h)")
	flag.Parse()

	if err := godotenv.Load(); err != nil {
		slog.Info("No .env file found, using defaults")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, *window); err != nil {
		slog.Error("report failed", slog.Any("err", err))
		os.Exit(1)
	}
}
