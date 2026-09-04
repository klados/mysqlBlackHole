package sqlemulate

import "fmt"

const (
	KeySupportedDBs = "supported_dbs"
	KeyMySQLUsers   = "mysql_users"
)

type MySQLUser struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type SupportedDbs struct {
	Name   string   `yaml:"name"`
	Users  []string `yaml:"users"`
	Tables []string `yaml:"tables"`
}

func GetSupportedDBUsersKey(db string) string {
	return fmt.Sprintf("%s:%s:users", KeySupportedDBs, db)
}

func GetAllowedDBTablesKey(db string) string {
	return fmt.Sprintf("%s:%s:tables", KeySupportedDBs, db)
}
