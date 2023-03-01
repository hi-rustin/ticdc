// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package conn

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-semver/semver"
	gmysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-sql-driver/mysql"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/dumpling/export"
	"github.com/pingcap/tidb/parser"
	tmysql "github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/util/dbutil"
	"github.com/pingcap/tidb/util/filter"
	"github.com/pingcap/tidb/util/regexpr-router"
	tcontext "github.com/pingcap/tiflow/dm/pkg/context"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/pingcap/tiflow/dm/pkg/terror"
	"go.uber.org/zap"
)

const (
	// DefaultDBTimeout represents a DB operation timeout for common usages.
	DefaultDBTimeout = 30 * time.Second

	// for MariaDB, UUID set as `gtid_domain_id` + domainServerIDSeparator + `server_id`.
	domainServerIDSeparator = "-"

	// the default base(min) server id generated by random.
	defaultBaseServerID = math.MaxUint32 / 10
)

// GetFlavor gets flavor from DB.
func GetFlavor(ctx context.Context, db *BaseDB) (string, error) {
	value, err := dbutil.ShowVersion(ctx, db.DB)
	if err != nil {
		return "", terror.DBErrorAdapt(err, db.Scope, terror.ErrDBDriverError)
	}
	if IsMariaDB(value) {
		return gmysql.MariaDBFlavor, nil
	}
	return gmysql.MySQLFlavor, nil
}

// GetAllServerID gets all slave server id and master server id.
func GetAllServerID(ctx *tcontext.Context, db *BaseDB) (map[uint32]struct{}, error) {
	serverIDs, err := GetSlaveServerID(ctx, db)
	if err != nil {
		return nil, err
	}

	masterServerID, err := GetServerID(ctx, db)
	if err != nil {
		return nil, err
	}

	serverIDs[masterServerID] = struct{}{}
	return serverIDs, nil
}

// GetRandomServerID gets a random server ID which is not used.
func GetRandomServerID(ctx *tcontext.Context, db *BaseDB) (uint32, error) {
	rand.Seed(time.Now().UnixNano())

	serverIDs, err := GetAllServerID(ctx, db)
	if err != nil {
		return 0, err
	}

	for i := 0; i < 99999; i++ {
		randomValue := uint32(rand.Intn(100000))
		randomServerID := uint32(defaultBaseServerID) + randomValue
		if _, ok := serverIDs[randomServerID]; ok {
			continue
		}

		return randomServerID, nil
	}

	// should never happened unless the master has too many slave.
	return 0, terror.ErrInvalidServerID.Generatef("can't find a random available server ID")
}

// GetSlaveServerID gets all slave server id.
func GetSlaveServerID(ctx *tcontext.Context, db *BaseDB) (map[uint32]struct{}, error) {
	// need REPLICATION SLAVE privilege
	rows, err := db.QueryContext(ctx, `SHOW SLAVE HOSTS`)
	if err != nil {
		return nil, terror.DBErrorAdapt(err, db.Scope, terror.ErrDBDriverError)
	}
	defer rows.Close()

	/*
		in MySQL:
		mysql> SHOW SLAVE HOSTS;
		+------------+-----------+------+-----------+--------------------------------------+
		| Server_id  | Host      | Port | Master_id | Slave_UUID                           |
		+------------+-----------+------+-----------+--------------------------------------+
		|  192168010 | iconnect2 | 3306 | 192168011 | 14cb6624-7f93-11e0-b2c0-c80aa9429562 |
		| 1921680101 | athena    | 3306 | 192168011 | 07af4990-f41f-11df-a566-7ac56fdaf645 |
		+------------+-----------+------+-----------+--------------------------------------+

		in MariaDB:
		mysql> SHOW SLAVE HOSTS;
		+------------+-----------+------+-----------+
		| Server_id  | Host      | Port | Master_id |
		+------------+-----------+------+-----------+
		|  192168010 | iconnect2 | 3306 | 192168011 |
		| 1921680101 | athena    | 3306 | 192168011 |
		+------------+-----------+------+-----------+
	*/

	serverIDs := make(map[uint32]struct{})
	var rowsResult []string
	rowsResult, err = export.GetSpecifiedColumnValueAndClose(rows, "Server_id")
	if err != nil {
		return nil, terror.DBErrorAdapt(err, db.Scope, terror.ErrDBDriverError)
	}
	for _, serverID := range rowsResult {
		// serverID will not be null
		serverIDUInt, err := strconv.ParseUint(serverID, 10, 32)
		if err != nil {
			return nil, terror.DBErrorAdapt(err, db.Scope, terror.ErrDBDriverError)
		}
		serverIDs[uint32(serverIDUInt)] = struct{}{}
	}
	return serverIDs, nil
}

// GetSessionVariable gets connection's session variable.
func GetSessionVariable(ctx *tcontext.Context, conn *BaseConn, variable string) (value string, err error) {
	failpoint.Inject("GetSessionVariableFailed", func(val failpoint.Value) {
		items := strings.Split(val.(string), ",")
		if len(items) != 2 {
			log.L().Fatal("failpoint GetSessionVariableFailed's value is invalid", zap.String("val", val.(string)))
		}
		variableName := items[0]
		errCode, err1 := strconv.ParseUint(items[1], 10, 16)
		if err1 != nil {
			log.L().Fatal("failpoint GetSessionVariableFailed's value is invalid", zap.String("val", val.(string)))
		}
		if variable == variableName {
			err = tmysql.NewErr(uint16(errCode))
			log.L().Warn("GetSessionVariable failed", zap.String("variable", variable), zap.String("failpoint", "GetSessionVariableFailed"), zap.Error(err))
			failpoint.Return("", terror.DBErrorAdapt(err, conn.Scope, terror.ErrDBDriverError))
		}
	})
	return getVariable(ctx, conn, variable, false)
}

// GetServerID gets server's `server_id`.
func GetServerID(ctx *tcontext.Context, db *BaseDB) (uint32, error) {
	serverIDStr, err := GetGlobalVariable(ctx, db, "server_id")
	if err != nil {
		return 0, err
	}

	serverID, err := strconv.ParseUint(serverIDStr, 10, 32)
	return uint32(serverID), terror.ErrInvalidServerID.Delegate(err, serverIDStr)
}

// GetMariaDBGtidDomainID gets MariaDB server's `gtid_domain_id`.
func GetMariaDBGtidDomainID(ctx *tcontext.Context, db *BaseDB) (uint32, error) {
	domainIDStr, err := GetGlobalVariable(ctx, db, "gtid_domain_id")
	if err != nil {
		return 0, err
	}

	domainID, err := strconv.ParseUint(domainIDStr, 10, 32)
	return uint32(domainID), terror.ErrMariaDBDomainID.Delegate(err, domainIDStr)
}

// GetServerUUID gets server's `server_uuid`.
func GetServerUUID(ctx *tcontext.Context, db *BaseDB, flavor string) (string, error) {
	if flavor == gmysql.MariaDBFlavor {
		return GetMariaDBUUID(ctx, db)
	}
	serverUUID, err := GetGlobalVariable(ctx, db, "server_uuid")
	return serverUUID, err
}

// GetServerUnixTS gets server's `UNIX_TIMESTAMP()`.
func GetServerUnixTS(ctx context.Context, db *BaseDB) (int64, error) {
	var ts int64
	row := db.DB.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP()")
	err := row.Scan(&ts)
	if err != nil {
		log.L().Error("can't SELECT UNIX_TIMESTAMP()", zap.Error(err))
		return ts, terror.DBErrorAdapt(err, db.Scope, terror.ErrDBDriverError)
	}
	return ts, err
}

// GetMariaDBUUID gets equivalent `server_uuid` for MariaDB
// `gtid_domain_id` joined `server_id` with domainServerIDSeparator.
func GetMariaDBUUID(ctx *tcontext.Context, db *BaseDB) (string, error) {
	domainID, err := GetMariaDBGtidDomainID(ctx, db)
	if err != nil {
		return "", err
	}
	serverID, err := GetServerID(ctx, db)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d%s%d", domainID, domainServerIDSeparator, serverID), nil
}

// GetParser gets a parser for sql.DB which is suitable for session variable sql_mode.
func GetParser(ctx *tcontext.Context, db *BaseDB) (*parser.Parser, error) {
	c, err := db.GetBaseConn(ctx.Ctx)
	if err != nil {
		return nil, err
	}
	defer db.CloseConnWithoutErr(c)
	return GetParserForConn(ctx, c)
}

// GetParserForConn gets a parser for BaseConn which is suitable for session variable sql_mode.
func GetParserForConn(ctx *tcontext.Context, conn *BaseConn) (*parser.Parser, error) {
	sqlMode, err := GetSessionVariable(ctx, conn, "sql_mode")
	if err != nil {
		return nil, err
	}
	return GetParserFromSQLModeStr(sqlMode)
}

// GetParserFromSQLModeStr gets a parser and applies given sqlMode.
func GetParserFromSQLModeStr(sqlMode string) (*parser.Parser, error) {
	mode, err := tmysql.GetSQLMode(sqlMode)
	if err != nil {
		return nil, err
	}

	parser2 := parser.New()
	parser2.SetSQLMode(mode)
	return parser2, nil
}

// KillConn kills the DB connection (thread in mysqld).
func KillConn(ctx *tcontext.Context, db *BaseDB, connID uint32) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf("KILL %d", connID))
	return terror.DBErrorAdapt(err, db.Scope, terror.ErrDBDriverError)
}

// IsMySQLError checks whether err is MySQLError error.
func IsMySQLError(err error, code uint16) bool {
	err = errors.Cause(err)
	e, ok := err.(*mysql.MySQLError)
	return ok && e.Number == code
}

// IsErrDuplicateEntry checks whether err is DuplicateEntry error.
func IsErrDuplicateEntry(err error) bool {
	return IsMySQLError(err, tmysql.ErrDupEntry)
}

// IsErrBinlogPurged checks whether err is BinlogPurged error.
func IsErrBinlogPurged(err error) bool {
	return IsMySQLError(err, tmysql.ErrMasterFatalErrorReadingBinlog)
}

// IsNoSuchThreadError checks whether err is NoSuchThreadError.
func IsNoSuchThreadError(err error) bool {
	return IsMySQLError(err, tmysql.ErrNoSuchThread)
}

// GetGTIDMode return GTID_MODE.
func GetGTIDMode(ctx *tcontext.Context, db *BaseDB) (string, error) {
	val, err := GetGlobalVariable(ctx, db, "GTID_MODE")
	return val, err
}

// ExtractTiDBVersion extract tidb's version
// version format: "5.7.25-TiDB-v3.0.0-beta-211-g09beefbe0-dirty"
// -                            ^..........
func ExtractTiDBVersion(version string) (*semver.Version, error) {
	versions := strings.Split(strings.TrimSuffix(version, "-dirty"), "-")
	end := len(versions)
	switch end {
	case 3, 4:
	case 5, 6:
		end -= 2
	default:
		return nil, errors.Errorf("not a valid TiDB version: %s", version)
	}
	rawVersion := strings.Join(versions[2:end], "-")
	rawVersion = strings.TrimPrefix(rawVersion, "v")
	return semver.NewVersion(rawVersion)
}

// AddGSetWithPurged is used to handle this case: https://github.com/pingcap/dm/issues/1418
// we might get a gtid set from Previous_gtids event in binlog, but that gtid set can't be used to start a gtid sync
// because it doesn't cover all gtid_purged. The error of using it will be
// ERROR 1236 (HY000): The slave is connecting using CHANGE MASTER TO MASTER_AUTO_POSITION = 1, but the master has purged binary logs containing GTIDs that the slave requires.
// so we add gtid_purged to it.
func AddGSetWithPurged(ctx context.Context, gset gmysql.GTIDSet, conn *BaseConn) (gmysql.GTIDSet, error) {
	if _, ok := gset.(*gmysql.MariadbGTIDSet); ok {
		return gset, nil
	}

	var (
		gtidStr string
		row     *sql.Row
		err     error
	)

	failpoint.Inject("GetGTIDPurged", func(val failpoint.Value) {
		str := val.(string)
		gtidStr = str
		failpoint.Goto("bypass")
	})
	row = conn.DBConn.QueryRowContext(ctx, "select @@GLOBAL.gtid_purged")
	err = row.Scan(&gtidStr)
	if err != nil {
		log.L().Error("can't get @@GLOBAL.gtid_purged when try to add it to gtid set", zap.Error(err))
		return gset, terror.DBErrorAdapt(err, conn.Scope, terror.ErrDBDriverError)
	}
	failpoint.Label("bypass")
	if gtidStr == "" {
		return gset, nil
	}

	cloned := gset.Clone()
	err = cloned.Update(gtidStr)
	if err != nil {
		return nil, err
	}
	return cloned, nil
}

// AdjustSQLModeCompatible adjust downstream sql mode to compatible.
// TODO: When upstream's datatime is 2020-00-00, 2020-00-01, 2020-06-00
// and so on, downstream will be 2019-11-30, 2019-12-01, 2020-05-31,
// as if set the 'NO_ZERO_IN_DATE', 'NO_ZERO_DATE'.
// This is because the implementation of go-mysql, that you can see
// https://github.com/go-mysql-org/go-mysql/blob/master/replication/row_event.go#L1063-L1087
func AdjustSQLModeCompatible(sqlModes string) (string, error) {
	needDisable := []string{
		"NO_ZERO_IN_DATE",
		"NO_ZERO_DATE",
		"ERROR_FOR_DIVISION_BY_ZERO",
		"NO_AUTO_CREATE_USER",
		"STRICT_TRANS_TABLES",
		"STRICT_ALL_TABLES",
	}
	needEnable := []string{
		"IGNORE_SPACE",
		"NO_AUTO_VALUE_ON_ZERO",
		"ALLOW_INVALID_DATES",
	}
	disable := strings.Join(needDisable, ",")
	enable := strings.Join(needEnable, ",")

	mode, err := tmysql.GetSQLMode(sqlModes)
	if err != nil {
		return sqlModes, err
	}
	disableMode, err2 := tmysql.GetSQLMode(disable)
	if err2 != nil {
		return sqlModes, err2
	}
	enableMode, err3 := tmysql.GetSQLMode(enable)
	if err3 != nil {
		return sqlModes, err3
	}
	// About this bit manipulation, details can be seen
	// https://github.com/pingcap/dm/pull/1869#discussion_r669771966
	mode = (mode &^ disableMode) | enableMode

	return GetSQLModeStrBySQLMode(mode), nil
}

// GetSQLModeStrBySQLMode get string represent of sql_mode by sql_mode.
func GetSQLModeStrBySQLMode(sqlMode tmysql.SQLMode) string {
	var sqlModeStr []string
	for str, SQLMode := range tmysql.Str2SQLMode {
		if sqlMode&SQLMode != 0 {
			sqlModeStr = append(sqlModeStr, str)
		}
	}
	return strings.Join(sqlModeStr, ",")
}

// GetMaxConnections gets max_connections for sql.DB which is suitable for session variable max_connections.
func GetMaxConnections(ctx *tcontext.Context, db *BaseDB) (int, error) {
	c, err := db.GetBaseConn(ctx.Ctx)
	if err != nil {
		return 0, err
	}
	defer db.CloseConnWithoutErr(c)
	return GetMaxConnectionsForConn(ctx, c)
}

// GetMaxConnectionsForConn gets max_connections for BaseConn which is suitable for session variable max_connections.
func GetMaxConnectionsForConn(ctx *tcontext.Context, conn *BaseConn) (int, error) {
	maxConnectionsStr, err := GetSessionVariable(ctx, conn, "max_connections")
	if err != nil {
		return 0, err
	}
	maxConnections, err := strconv.ParseUint(maxConnectionsStr, 10, 32)
	return int(maxConnections), err
}

// IsMariaDB tells whether the version is mariadb.
func IsMariaDB(version string) bool {
	return strings.Contains(strings.ToUpper(version), "MARIADB")
}

// CreateTableSQLToOneRow formats the result of SHOW CREATE TABLE to one row.
func CreateTableSQLToOneRow(sql string) string {
	sql = strings.ReplaceAll(sql, "\n", "")
	sql = strings.ReplaceAll(sql, "  ", " ")
	return sql
}

// FetchAllDoTables returns all need to do tables after filtered (fetches from upstream MySQL).
func FetchAllDoTables(ctx context.Context, db *BaseDB, bw *filter.Filter) (map[string][]string, error) {
	schemas, err := dbutil.GetSchemas(ctx, db.DB)

	failpoint.Inject("FetchAllDoTablesFailed", func(val failpoint.Value) {
		err = tmysql.NewErr(uint16(val.(int)))
		log.L().Warn("FetchAllDoTables failed", zap.String("failpoint", "FetchAllDoTablesFailed"), zap.Error(err))
	})

	if err != nil {
		return nil, terror.WithScope(err, db.Scope)
	}

	ftSchemas := make([]*filter.Table, 0, len(schemas))
	for _, schema := range schemas {
		if filter.IsSystemSchema(schema) {
			continue
		}
		ftSchemas = append(ftSchemas, &filter.Table{
			Schema: schema,
			Name:   "", // schema level
		})
	}
	ftSchemas = bw.Apply(ftSchemas)
	if len(ftSchemas) == 0 {
		log.L().Warn("no schema need to sync")
		return nil, nil
	}

	schemaToTables := make(map[string][]string)
	for _, ftSchema := range ftSchemas {
		schema := ftSchema.Schema
		// use `GetTables` from tidb-tools, no view included
		tables, err := dbutil.GetTables(ctx, db.DB, schema)
		if err != nil {
			return nil, terror.DBErrorAdapt(err, db.Scope, terror.ErrDBDriverError)
		}
		ftTables := make([]*filter.Table, 0, len(tables))
		for _, table := range tables {
			ftTables = append(ftTables, &filter.Table{
				Schema: schema,
				Name:   table,
			})
		}
		ftTables = bw.Apply(ftTables)
		if len(ftTables) == 0 {
			log.L().Info("no tables need to sync", zap.String("schema", schema))
			continue // NOTE: should we still keep it as an empty elem?
		}
		tables = tables[:0]
		for _, ftTable := range ftTables {
			tables = append(tables, ftTable.Name)
		}
		schemaToTables[schema] = tables
	}

	return schemaToTables, nil
}

// FetchTargetDoTables returns all need to do tables after filtered and routed (fetches from upstream MySQL).
func FetchTargetDoTables(
	ctx context.Context,
	source string,
	db *BaseDB,
	bw *filter.Filter,
	router *regexprrouter.RouteTable,
) (map[filter.Table][]filter.Table, map[filter.Table][]string, error) {
	// fetch tables from source and filter them
	sourceTables, err := FetchAllDoTables(ctx, db, bw)

	failpoint.Inject("FetchTargetDoTablesFailed", func(val failpoint.Value) {
		err = tmysql.NewErr(uint16(val.(int)))
		log.L().Warn("FetchTargetDoTables failed", zap.String("failpoint", "FetchTargetDoTablesFailed"), zap.Error(err))
	})

	if err != nil {
		return nil, nil, err
	}

	tableMapper := make(map[filter.Table][]filter.Table)
	extendedColumnPerTable := make(map[filter.Table][]string)
	for schema, tables := range sourceTables {
		for _, table := range tables {
			targetSchema, targetTable, err := router.Route(schema, table)
			if err != nil {
				return nil, nil, terror.ErrGenTableRouter.Delegate(err)
			}

			target := filter.Table{
				Schema: targetSchema,
				Name:   targetTable,
			}
			tableMapper[target] = append(tableMapper[target], filter.Table{
				Schema: schema,
				Name:   table,
			})
			col, _ := router.FetchExtendColumn(schema, table, source)
			if len(col) > 0 {
				extendedColumnPerTable[target] = col
			}
		}
	}

	return tableMapper, extendedColumnPerTable, nil
}