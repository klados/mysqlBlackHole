package main

import (
	"context"
	"embed"
	"encoding/json"

	"log/slog"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"mysqlBlackHole/model/sqlemulate"
)

//go:embed seed_data.yaml
var configFS embed.FS

type Config struct {
	SupportedDBs []sqlemulate.SupportedDbs `yaml:"supported_dbs"`
	MySQLUsers   []sqlemulate.MySQLUser    `yaml:"mysql_users"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(slog.String("service", "seed-redis")))

	if err := godotenv.Load(); err != nil {
		slog.Info("No .env file found, using defaults")
	}

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("failed to load config", slog.Any("err", err))
		os.Exit(1)
	}

	// HONEYPOT_USERS (format "user:pass,user2:pass2") overrides the public
	// decoy credentials in seed_data.yaml. Use this in production so the
	// published passwords are never the live ones. Never log passwords.
	// A set-but-unparseable value is a hard error: silently reseeding the
	// public decoys would leave the honeypot on known credentials.
	rawUsers, usersSet := os.LookupEnv("HONEYPOT_USERS")
	override, skipped := parseUsers(rawUsers)
	if usersSet && strings.TrimSpace(rawUsers) != "" && len(override) == 0 {
		slog.Error("HONEYPOT_USERS is set but no valid entries parsed; refusing to seed public decoys",
			slog.Int("skipped", skipped))
		os.Exit(1)
	}
	if len(override) > 0 {
		names := make([]string, 0, len(override))
		for _, u := range override {
			names = append(names, u.Username)
		}
		slog.Info("overriding decoy mysql_users from HONEYPOT_USERS",
			slog.Int("count", len(override)),
			slog.Int("skipped", skipped),
			slog.Any("users", names))
		cfg.MySQLUsers = override
	} else if skipped > 0 {
		slog.Warn("HONEYPOT_USERS had only malformed entries; using YAML decoys",
			slog.Int("skipped", skipped))
	}

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()

	if err := client.Ping(ctx).Err(); err != nil {
		slog.Error("failed to connect to redis", slog.String("addr", addr), slog.Any("err", err))
		os.Exit(1)
	}
	slog.Info("connected to redis", slog.String("addr", addr))
	defer client.Close()

	if err := seedSupportedDBs(ctx, client, cfg.SupportedDBs); err != nil {
		slog.Error("failed to seed", slog.String("key", sqlemulate.KeySupportedDBs), slog.Any("err", err))
		os.Exit(1)
	}
	if err := seedMySQLUsers(ctx, client, cfg.MySQLUsers); err != nil {
		slog.Error("failed to seed", slog.String("key", sqlemulate.KeyMySQLUsers), slog.Any("err", err))
		os.Exit(1)
	}

	slog.Info("redis seeding complete")
}

func seedSupportedDBs(ctx context.Context, client *redis.Client, allowedDBs []sqlemulate.SupportedDbs) error {
	if err := client.Del(ctx, sqlemulate.KeySupportedDBs).Err(); err != nil {
		return err
	}
	if err := deleteKeysByPattern(ctx, client, sqlemulate.KeySupportedDBs+":"); err != nil {
		return err
	}
	if err := deleteKeysByPattern(ctx, client, "table_data:*"); err != nil {
		return err
	}

	dbNames := make([]any, 0, len(allowedDBs))
	for _, db := range allowedDBs {
		dbNames = append(dbNames, db.Name)
	}
	if err := client.SAdd(ctx, sqlemulate.KeySupportedDBs, dbNames...).Err(); err != nil {
		return err
	}

	for _, db := range allowedDBs {
		usersKey := sqlemulate.GetSupportedDBUsersKey(db.Name)
		tablesKey := sqlemulate.GetAllowedDBTablesKey(db.Name)

		if len(db.Users) > 0 {
			if err := client.SAdd(ctx, usersKey, stringSliceToAny(db.Users)...).Err(); err != nil {
				return err
			}
		}

		if len(db.Tables) > 0 {
			if err := client.SAdd(ctx, tablesKey, stringSliceToAny(db.Tables)...).Err(); err != nil {
				return err
			}
		}

		if err := seedTableData(ctx, client, db.Name, db.TableData); err != nil {
			return err
		}
	}

	return nil
}

func seedTableData(ctx context.Context, client *redis.Client, dbName string, tableData map[string]sqlemulate.TableData) error {
	for table, data := range tableData {
		if len(data.Columns) == 0 {
			continue
		}

		payload, err := json.Marshal(data)
		if err != nil {
			return err
		}

		if err := client.Set(ctx, sqlemulate.GetTableDataKey(dbName, table), payload, 0).Err(); err != nil {
			return err
		}
	}

	return nil
}

func deleteKeysByPattern(ctx context.Context, client *redis.Client, pattern string) error {
	iter := client.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		if err := client.Del(ctx, iter.Val()).Err(); err != nil {
			return err
		}
	}

	return iter.Err()
}

func seedMySQLUsers(ctx context.Context, client *redis.Client, mysqlUsers []sqlemulate.MySQLUser) error {
	if err := client.Del(ctx, sqlemulate.KeyMySQLUsers).Err(); err != nil {
		return err
	}

	if len(mysqlUsers) == 0 {
		return nil
	}

	fields := make(map[string]any, len(mysqlUsers))
	for _, u := range mysqlUsers {
		fields[u.Username] = u.Password
	}

	return client.HSet(ctx, sqlemulate.KeyMySQLUsers, fields).Err()
}

func loadConfig() (*Config, error) {
	data, err := configFS.ReadFile("seed_data.yaml")
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// usersFromEnv parses HONEYPOT_USERS ("user:pass,user2:pass2") into MySQLUser
// entries. Malformed pairs are skipped. Returns nil when unset so callers keep
// the YAML decoys.
func usersFromEnv() []sqlemulate.MySQLUser {
	users, _ := parseUsers(os.Getenv("HONEYPOT_USERS"))
	return users
}

// parseUsers splits raw ("user:pass,user2:pass2") on commas, then on the first
// colon. It returns the valid entries plus the count of skipped malformed
// pairs. Passwords may contain colons but not commas.
func parseUsers(raw string) ([]sqlemulate.MySQLUser, int) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, 0
	}
	var out []sqlemulate.MySQLUser
	skipped := 0
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		user, pass, ok := strings.Cut(pair, ":")
		user = strings.TrimSpace(user)
		pass = strings.TrimSpace(pass)
		if !ok || user == "" || pass == "" {
			slog.Warn("skipping malformed HONEYPOT_USERS entry")
			skipped++
			continue
		}
		out = append(out, sqlemulate.MySQLUser{Username: user, Password: pass})
	}
	return out, skipped
}

func stringSliceToAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}
