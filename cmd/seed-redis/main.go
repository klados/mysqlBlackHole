package main

import (
	"context"
	"embed"

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
	AllowedDBs []string               `yaml:"allowed_dbs"`
	MySQLUsers []sqlemulate.MySQLUser `yaml:"mysql_users"`
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

	if err := seedAllowedDBs(ctx, client, cfg.AllowedDBs); err != nil {
		log.Fatalf("failed to seed %s: %v", sqlemulate.KeyAllowedDBs, err)
	}
	if err := seedMySQLUsers(ctx, client, cfg.MySQLUsers); err != nil {
		log.Fatalf("failed to seed %s: %v", sqlemulate.KeyMySQLUsers, err)
	}

	log.Println("redis seeding complete")
}

func seedAllowedDBs(ctx context.Context, client *redis.Client, allowedDBs []string) error {
	if err := client.Del(ctx, sqlemulate.KeyAllowedDBs).Err(); err != nil {
		return err
	}
	return client.RPush(ctx, sqlemulate.KeyAllowedDBs, stringSliceToAny(allowedDBs)...).Err()
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
