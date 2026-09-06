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
	Name      string               `yaml:"name"`
	Users     []string             `yaml:"users"`
	Tables    []string             `yaml:"tables"`
	TableData map[string]TableData `yaml:"table_data"`
}

type TableData struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

func GetSupportedDBUsersKey(db string) string {
	return fmt.Sprintf("%s:%s:users", KeySupportedDBs, db)
}

func GetAllowedDBTablesKey(db string) string {
	return fmt.Sprintf("%s:%s:tables", KeySupportedDBs, db)
}

func GetTableDataKey(db, table string) string {
	return fmt.Sprintf("table_data:%s:%s", db, table)
}
