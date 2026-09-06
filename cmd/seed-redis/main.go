package main

import (
	"context"
	"embed"
	"encoding/json"

	"log"
	"os"

	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"

	"mysqlBlackHole/model/sqlemulate"
)

//go:embed config.yaml
var configFS embed.FS

type Config struct {
	SupportedDBs []sqlemulate.SupportedDbs `yaml:"supported_dbs"`
	MySQLUsers   []sqlemulate.MySQLUser    `yaml:"mysql_users"`
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using defaults")
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}

	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()

	if err := client.Ping(ctx).Err(); err != nil {
		log.Fatalf("failed to connect to redis at %s: %v", addr, err)
	}
	log.Printf("connected to redis at %s", addr)
	defer client.Close()

	if err := seedSupportedDBs(ctx, client, cfg.SupportedDBs); err != nil {
		log.Fatalf("failed to seed %s: %v", sqlemulate.KeySupportedDBs, err)
	}
	if err := seedMySQLUsers(ctx, client, cfg.MySQLUsers); err != nil {
		log.Fatalf("failed to seed %s: %v", sqlemulate.KeyMySQLUsers, err)
	}

	log.Println("redis seeding complete")
}

func seedSupportedDBs(ctx context.Context, client *redis.Client, allowedDBs []sqlemulate.SupportedDbs) error {
	if err := client.Del(ctx, sqlemulate.KeySupportedDBs).Err(); err != nil {
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

		if err := client.Del(ctx, usersKey, tablesKey).Err(); err != nil {
			return err
		}

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
	if err := deleteTableDataKeys(ctx, client, dbName); err != nil {
		return err
	}

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

func deleteTableDataKeys(ctx context.Context, client *redis.Client, dbName string) error {
	pattern := sqlemulate.GetTableDataKey(dbName, "*")
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
	data, err := configFS.ReadFile("config.yaml")
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func stringSliceToAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}
