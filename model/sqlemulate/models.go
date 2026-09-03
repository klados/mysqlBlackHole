package sqlemulate

const (
	KeyAllowedDBs = "allowed_dbs"
	KeyMySQLUsers = "mysql_users"
)

type MySQLUser struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
