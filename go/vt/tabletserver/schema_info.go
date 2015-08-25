// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tabletserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	log "github.com/golang/glog"
	"github.com/youtube/vitess/go/acl"
	"github.com/youtube/vitess/go/cache"
	mproto "github.com/youtube/vitess/go/mysql/proto"
	"github.com/youtube/vitess/go/sqldb"
	"github.com/youtube/vitess/go/stats"
	"github.com/youtube/vitess/go/timer"
	"github.com/youtube/vitess/go/vt/schema"
	"github.com/youtube/vitess/go/vt/tableacl"
	"github.com/youtube/vitess/go/vt/tabletserver/planbuilder"
	"golang.org/x/net/context"
)

const baseShowTables = "select table_name, table_type, unix_timestamp(create_time), table_comment from information_schema.tables where table_schema = database()"

const maxTableCount = 10000

const (
	debugQueryPlansKey = "query_plans"
	debugQueryStatsKey = "query_stats"
	debugTableStatsKey = "table_stats"
	debugSchemaKey     = "schema"
)

// ExecPlan wraps the planbuilder's exec plan to enforce additional rules
// and track stats.
type ExecPlan struct {
	*planbuilder.ExecPlan
	TableInfo  *TableInfo
	Fields     []mproto.Field
	Rules      *QueryRules
	Authorized *tableacl.ACLResult

	mu         sync.Mutex
	QueryCount int64
	Time       time.Duration
	RowCount   int64
	ErrorCount int64
}

// Size allows ExecPlan to be in cache.LRUCache.
func (*ExecPlan) Size() int {
	return 1
}

// AddStats updates the stats for the current ExecPlan.
func (ep *ExecPlan) AddStats(queryCount int64, duration time.Duration, rowCount, errorCount int64) {
	ep.mu.Lock()
	ep.QueryCount += queryCount
	ep.Time += duration
	ep.RowCount += rowCount
	ep.ErrorCount += errorCount
	ep.mu.Unlock()
}

// Stats returns the current stats of ExecPlan.
func (ep *ExecPlan) Stats() (queryCount int64, duration time.Duration, rowCount, errorCount int64) {
	ep.mu.Lock()
	queryCount = ep.QueryCount
	duration = ep.Time
	rowCount = ep.RowCount
	errorCount = ep.ErrorCount
	ep.mu.Unlock()
	return
}

// SchemaOverride is a way to specify how the schema loaded by SchemaInfo
// must be overridden. Name is the name of the table, PKColumns specifies
// the new prmiary keys. Cache.Type specifies the rowcache operation for
// the table. It can be "R", which is read-only, or "RW" for read-write, and
// Table specifies the rowcache table to operate on.
// The purpose of this override is mainly to allow views to benefit from
// the rowcache. It has its downsides. Use carefully.
type SchemaOverride struct {
	Name      string
	PKColumns []string
	Cache     *struct {
		Type  string
		Table string
	}
}

// SchemaInfo stores the schema info and performs operations that
// keep itself and the rowcache up-to-date.
type SchemaInfo struct {
	mu                sync.Mutex
	tables            map[string]*TableInfo
	overrides         []SchemaOverride
	queries           *cache.LRUCache
	connPool          *ConnPool
	cachePool         *CachePool
	lastChange        time.Time
	ticks             *timer.Timer
	reloadTime        time.Duration
	endpoints         map[string]string
	queryServiceStats *QueryServiceStats
}

// NewSchemaInfo creates a new SchemaInfo.
func NewSchemaInfo(
	queryCacheSize int,
	statsPrefix string,
	endpoints map[string]string,
	reloadTime time.Duration,
	idleTimeout time.Duration,
	enablePublishStats bool,
	queryServiceStats *QueryServiceStats) *SchemaInfo {
	si := &SchemaInfo{
		queries:    cache.NewLRUCache(int64(queryCacheSize)),
		connPool:   NewConnPool("", 2, idleTimeout, enablePublishStats, queryServiceStats),
		ticks:      timer.NewTimer(reloadTime),
		endpoints:  endpoints,
		reloadTime: reloadTime,
	}
	if enablePublishStats {
		stats.Publish(statsPrefix+"QueryCacheLength", stats.IntFunc(si.queries.Length))
		stats.Publish(statsPrefix+"QueryCacheSize", stats.IntFunc(si.queries.Size))
		stats.Publish(statsPrefix+"QueryCacheCapacity", stats.IntFunc(si.queries.Capacity))
		stats.Publish(statsPrefix+"QueryCacheOldest", stats.StringFunc(func() string {
			return fmt.Sprintf("%v", si.queries.Oldest())
		}))
		stats.Publish(statsPrefix+"SchemaReloadTime", stats.DurationFunc(si.ticks.Interval))
		_ = stats.NewMultiCountersFunc(statsPrefix+"RowcacheStats", []string{"Table", "Stats"}, si.getRowcacheStats)
		_ = stats.NewMultiCountersFunc(statsPrefix+"RowcacheInvalidations", []string{"Table"}, si.getRowcacheInvalidations)
		_ = stats.NewMultiCountersFunc(statsPrefix+"QueryCounts", []string{"Table", "Plan"}, si.getQueryCount)
		_ = stats.NewMultiCountersFunc(statsPrefix+"QueryTimesNs", []string{"Table", "Plan"}, si.getQueryTime)
		_ = stats.NewMultiCountersFunc(statsPrefix+"QueryRowCounts", []string{"Table", "Plan"}, si.getQueryRowCount)
		_ = stats.NewMultiCountersFunc(statsPrefix+"QueryErrorCounts", []string{"Table", "Plan"}, si.getQueryErrorCount)
	}
	for _, ep := range endpoints {
		http.Handle(ep, si)
	}
	return si
}

// Open initializes the current SchemaInfo for service by loading the necessary info from the specified database.
func (si *SchemaInfo) Open(appParams, dbaParams *sqldb.ConnParams, schemaOverrides []SchemaOverride, cachePool *CachePool, strictMode bool) {
	ctx := context.Background()
	si.connPool.Open(appParams, dbaParams)
	// Get time first because it needs a connection from the pool.
	curTime := si.mysqlTime(ctx)

	conn := getOrPanic(ctx, si.connPool)
	defer conn.Recycle()

	if strictMode && !conn.VerifyStrict() {
		panic(NewTabletError(ErrFatal, "Could not verify strict mode"))
	}

	si.cachePool = cachePool
	tables, err := conn.Exec(ctx, baseShowTables, maxTableCount, false)
	if err != nil {
		panic(PrefixTabletError(ErrFatal, err, "Could not get table list: "))
	}

	si.tables = make(map[string]*TableInfo, len(tables.Rows))
	// TODO(sougou): Fix this in the parser.
	si.tables["dual"] = &TableInfo{Table: schema.NewTable("dual")}
	si.tables["DUAL"] = &TableInfo{Table: schema.NewTable("DUAL")}
	for _, row := range tables.Rows {
		tableName := row[0].String()
		tableInfo, err := NewTableInfo(
			conn,
			tableName,
			row[1].String(), // table_type
			row[2],          // create_time
			row[3].String(), // table_comment
			si.cachePool,
		)
		if err != nil {
			panic(PrefixTabletError(ErrFatal, err, fmt.Sprintf("Could not get load table %s: ", tableName)))
		}
		si.tables[tableName] = tableInfo
	}
	if schemaOverrides != nil {
		si.overrides = schemaOverrides
		si.override()
	}
	si.lastChange = curTime
	// Clear is not really needed. Doing it for good measure.
	si.queries.Clear()
	si.ticks.Start(func() { si.Reload() })
}

func (si *SchemaInfo) override() {
	for _, override := range si.overrides {
		table, ok := si.tables[override.Name]
		if !ok {
			log.Warningf("Table not found for override: %v", override)
			continue
		}
		if override.PKColumns != nil {
			if err := table.SetPK(override.PKColumns); err != nil {
				log.Warningf("%v: %v", err, override)
				continue
			}
		}
		if si.cachePool.IsClosed() || override.Cache == nil {
			continue
		}
		switch override.Cache.Type {
		case "RW":
			table.CacheType = schema.CACHE_RW
			table.Cache = NewRowCache(table, si.cachePool)
		case "W":
			table.CacheType = schema.CACHE_W
			if override.Cache.Table == "" {
				log.Warningf("Incomplete cache specs: %v", override)
				continue
			}
			totable, ok := si.tables[override.Cache.Table]
			if !ok {
				log.Warningf("Table not found: %v", override)
				continue
			}
			if totable.Cache == nil {
				log.Warningf("Table has no cache: %v", override)
				continue
			}
			table.Cache = totable.Cache
		default:
			log.Warningf("Ignoring cache override: %v", override)
		}
	}
}

// Close shuts down SchemaInfo. It can be re-opened after Close.
func (si *SchemaInfo) Close() {
	si.ticks.Stop()
	si.connPool.Close()
	si.tables = nil
	si.overrides = nil
	si.queries.Clear()
}

// Reload reloads the schema info from the db. Any tables that have changed
// since the last load are updated.
func (si *SchemaInfo) Reload() {
	defer logError(si.queryServiceStats)
	ctx := context.Background()
	// Get time first because it needs a connection from the pool.
	curTime := si.mysqlTime(ctx)

	var tables *mproto.QueryResult
	var err error
	func() {
		conn := getOrPanic(ctx, si.connPool)
		defer conn.Recycle()
		tables, err = conn.Exec(ctx, fmt.Sprintf("%s and unix_timestamp(create_time) >= %v", baseShowTables, si.lastChange.Unix()), maxTableCount, false)
	}()
	if err != nil {
		log.Warningf("Could not get table list for reload: %v", err)
		return
	}
	log.Infof("Reloading schema")
	for _, row := range tables.Rows {
		tableName := row[0].String()
		log.Infof("Reloading: %s", tableName)
		si.CreateOrUpdateTable(ctx, tableName)
	}
	si.lastChange = curTime
}

func (si *SchemaInfo) mysqlTime(ctx context.Context) time.Time {
	conn := getOrPanic(ctx, si.connPool)
	defer conn.Recycle()
	tm, err := conn.Exec(ctx, "select unix_timestamp()", 1, false)
	if err != nil {
		panic(PrefixTabletError(ErrFail, err, "Could not get MySQL time: "))
	}
	if len(tm.Rows) != 1 || len(tm.Rows[0]) != 1 || tm.Rows[0][0].IsNull() {
		panic(NewTabletError(ErrFail, "Unexpected result for MySQL time: %+v", tm.Rows))
	}
	t, err := strconv.ParseInt(tm.Rows[0][0].String(), 10, 64)
	if err != nil {
		panic(NewTabletError(ErrFail, "Could not parse time %+v: %v", tm, err))
	}
	return time.Unix(t, 0)
}

// safe to call this if Close has been called, as si.ticks will be stopped
// and won't fire
func (si *SchemaInfo) triggerReload() {
	si.ticks.Trigger()
}

// ClearQueryPlanCache should be called if query plan cache is potentially obsolete
func (si *SchemaInfo) ClearQueryPlanCache() {
	si.mu.Lock()
	defer si.mu.Unlock()
	si.queries.Clear()
}

// CreateOrUpdateTable must be called if a DDL was applied to that table.
func (si *SchemaInfo) CreateOrUpdateTable(ctx context.Context, tableName string) {
	si.mu.Lock()
	defer si.mu.Unlock()

	conn := getOrPanic(ctx, si.connPool)
	defer conn.Recycle()
	tables, err := conn.Exec(ctx, fmt.Sprintf("%s and table_name = '%s'", baseShowTables, tableName), 1, false)
	if err != nil {
		panic(PrefixTabletError(ErrFail, err, fmt.Sprintf("Error fetching table %s: ", tableName)))
	}
	if len(tables.Rows) != 1 {
		// This can happen if DDLs race with each other.
		return
	}
	tableInfo, err := NewTableInfo(
		conn,
		tableName,
		tables.Rows[0][1].String(), // table_type
		tables.Rows[0][2],          // create_time
		tables.Rows[0][3].String(), // table_comment
		si.cachePool,
	)
	if err != nil {
		// This can happen if DDLs race with each other.
		return
	}
	if _, ok := si.tables[tableName]; ok {
		// If the table already exists, we overwrite it with the latest info.
		// This also means that the query cache needs to be cleared.
		// Otherwise, the query plans may not be in sync with the schema.
		si.queries.Clear()
		log.Infof("Updating table %s", tableName)
	}
	si.tables[tableName] = tableInfo

	if tableInfo.CacheType == schema.CACHE_NONE {
		log.Infof("Initialized table: %s", tableName)
	} else {
		log.Infof("Initialized cached table: %s, prefix: %s", tableName, tableInfo.Cache.prefix)
	}

	// If the table has an override, re-apply all overrides.
	for _, o := range si.overrides {
		if o.Name == tableName {
			si.override()
			return
		}
	}
}

// DropTable must be called if a table was dropped.
func (si *SchemaInfo) DropTable(tableName string) {
	si.mu.Lock()
	defer si.mu.Unlock()

	delete(si.tables, tableName)
	si.queries.Clear()
	log.Infof("Table %s forgotten", tableName)
}

// GetPlan returns the ExecPlan that for the query. Plans are cached in a cache.LRUCache.
func (si *SchemaInfo) GetPlan(ctx context.Context, logStats *SQLQueryStats, sql string) *ExecPlan {
	// Fastpath if plan already exists.
	if plan := si.getQuery(sql); plan != nil {
		return plan
	}

	si.mu.Lock()
	defer si.mu.Unlock()
	// Recheck. A plan might have been built by someone else.
	if plan := si.getQuery(sql); plan != nil {
		return plan
	}

	var tableInfo *TableInfo
	GetTable := func(tableName string) (table *schema.Table, ok bool) {
		tableInfo, ok = si.tables[tableName]
		if !ok {
			return nil, false
		}
		return tableInfo.Table, true
	}
	splan, err := planbuilder.GetExecPlan(sql, GetTable)
	if err != nil {
		panic(PrefixTabletError(ErrFail, err, ""))
	}
	plan := &ExecPlan{ExecPlan: splan, TableInfo: tableInfo}
	plan.Rules = QueryRuleSources.filterByPlan(sql, plan.PlanId, plan.TableName)
	plan.Authorized = tableacl.Authorized(plan.TableName, plan.PlanId.MinRole())
	if plan.PlanId.IsSelect() {
		if plan.FieldQuery == nil {
			log.Warningf("Cannot cache field info: %s", sql)
		} else {
			conn := getOrPanic(ctx, si.connPool)
			defer conn.Recycle()
			sql := plan.FieldQuery.Query
			start := time.Now()
			r, err := conn.Exec(ctx, sql, 1, true)
			logStats.AddRewrittenSql(sql, start)
			if err != nil {
				panic(PrefixTabletError(ErrFail, err, "Error fetching fields: "))
			}
			plan.Fields = r.Fields
		}
	} else if plan.PlanId == planbuilder.PLAN_DDL || plan.PlanId == planbuilder.PLAN_SET {
		return plan
	}
	si.queries.Set(sql, plan)
	return plan
}

// GetStreamPlan is similar to GetPlan, but doesn't use the cache
// and doesn't enforce a limit. It just returns the parsed query.
func (si *SchemaInfo) GetStreamPlan(sql string) *ExecPlan {
	var tableInfo *TableInfo
	GetTable := func(tableName string) (table *schema.Table, ok bool) {
		si.mu.Lock()
		defer si.mu.Unlock()
		tableInfo, ok = si.tables[tableName]
		if !ok {
			return nil, false
		}
		return tableInfo.Table, true
	}
	splan, err := planbuilder.GetStreamExecPlan(sql, GetTable)
	if err != nil {
		panic(PrefixTabletError(ErrFail, err, ""))
	}
	plan := &ExecPlan{ExecPlan: splan, TableInfo: tableInfo}
	plan.Rules = QueryRuleSources.filterByPlan(sql, plan.PlanId, plan.TableName)
	plan.Authorized = tableacl.Authorized(plan.TableName, plan.PlanId.MinRole())
	return plan
}

// GetTable returns the TableInfo for a table.
func (si *SchemaInfo) GetTable(tableName string) *TableInfo {
	si.mu.Lock()
	defer si.mu.Unlock()
	return si.tables[tableName]
}

// GetSchema returns a copy of the schema.
func (si *SchemaInfo) GetSchema() []*schema.Table {
	si.mu.Lock()
	defer si.mu.Unlock()
	tables := make([]*schema.Table, 0, len(si.tables))
	for _, v := range si.tables {
		tables = append(tables, v.Table)
	}
	return tables
}

func (si *SchemaInfo) getQuery(sql string) *ExecPlan {
	if cacheResult, ok := si.queries.Get(sql); ok {
		return cacheResult.(*ExecPlan)
	}
	return nil
}

// SetQueryCacheSize sets the query cache size.
func (si *SchemaInfo) SetQueryCacheSize(size int) {
	if size <= 0 {
		panic(NewTabletError(ErrFail, "cache size %v out of range", size))
	}
	si.queries.SetCapacity(int64(size))
}

// SetReloadTime changes how often the schema is reloaded. This
// call also triggers an immediate reload.
func (si *SchemaInfo) SetReloadTime(reloadTime time.Duration) {
	si.ticks.Trigger()
	si.ticks.SetInterval(reloadTime)
	si.mu.Lock()
	defer si.mu.Unlock()
	si.reloadTime = reloadTime
}

// ReloadTime returns schema info reload time.
func (si *SchemaInfo) ReloadTime() time.Duration {
	si.mu.Lock()
	defer si.mu.Unlock()
	return si.reloadTime
}

func (si *SchemaInfo) getRowcacheStats() map[string]int64 {
	si.mu.Lock()
	defer si.mu.Unlock()
	tstats := make(map[string]int64)
	for k, v := range si.tables {
		if v.CacheType != schema.CACHE_NONE {
			hits, absent, misses, _ := v.Stats()
			tstats[k+".Hits"] = hits
			tstats[k+".Absent"] = absent
			tstats[k+".Misses"] = misses
		}
	}
	return tstats
}

func (si *SchemaInfo) getRowcacheInvalidations() map[string]int64 {
	si.mu.Lock()
	defer si.mu.Unlock()
	tstats := make(map[string]int64)
	for k, v := range si.tables {
		if v.CacheType != schema.CACHE_NONE {
			_, _, _, invalidations := v.Stats()
			tstats[k] = invalidations
		}
	}
	return tstats
}

func (si *SchemaInfo) getQueryCount() map[string]int64 {
	f := func(plan *ExecPlan) int64 {
		queryCount, _, _, _ := plan.Stats()
		return queryCount
	}
	return si.getQueryStats(f)
}

func (si *SchemaInfo) getQueryTime() map[string]int64 {
	f := func(plan *ExecPlan) int64 {
		_, time, _, _ := plan.Stats()
		return int64(time)
	}
	return si.getQueryStats(f)
}

func (si *SchemaInfo) getQueryRowCount() map[string]int64 {
	f := func(plan *ExecPlan) int64 {
		_, _, rowCount, _ := plan.Stats()
		return rowCount
	}
	return si.getQueryStats(f)
}

func (si *SchemaInfo) getQueryErrorCount() map[string]int64 {
	f := func(plan *ExecPlan) int64 {
		_, _, _, errorCount := plan.Stats()
		return errorCount
	}
	return si.getQueryStats(f)
}

type queryStatsFunc func(*ExecPlan) int64

func (si *SchemaInfo) getQueryStats(f queryStatsFunc) map[string]int64 {
	keys := si.queries.Keys()
	qstats := make(map[string]int64)
	for _, v := range keys {
		if plan := si.getQuery(v); plan != nil {
			table := plan.TableName
			if table == "" {
				table = "Join"
			}
			planType := plan.PlanId.String()
			data := f(plan)
			qstats[table+"."+planType] += data
		}
	}
	return qstats
}

type perQueryStats struct {
	Query      string
	Table      string
	Plan       planbuilder.PlanType
	QueryCount int64
	Time       time.Duration
	RowCount   int64
	ErrorCount int64
}

func (si *SchemaInfo) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if err := acl.CheckAccessHTTP(request, acl.DEBUGGING); err != nil {
		acl.SendError(response, err)
		return
	}
	if ep, ok := si.endpoints[debugQueryPlansKey]; ok && request.URL.Path == ep {
		si.handleHTTPQueryPlans(response, request)
	} else if ep, ok := si.endpoints[debugQueryStatsKey]; ok && request.URL.Path == ep {
		si.handleHTTPQueryStats(response, request)
	} else if ep, ok := si.endpoints[debugTableStatsKey]; ok && request.URL.Path == ep {
		si.handleHTTPTableStats(response, request)
	} else if ep, ok := si.endpoints[debugSchemaKey]; ok && request.URL.Path == ep {
		si.handleHTTPSchema(response, request)
	} else {
		response.WriteHeader(http.StatusNotFound)
	}
}

func (si *SchemaInfo) handleHTTPQueryPlans(response http.ResponseWriter, request *http.Request) {
	keys := si.queries.Keys()
	response.Header().Set("Content-Type", "text/plain")
	response.Write([]byte(fmt.Sprintf("Length: %d\n", len(keys))))
	for _, v := range keys {
		response.Write([]byte(fmt.Sprintf("%#v\n", v)))
		if plan := si.getQuery(v); plan != nil {
			if b, err := json.MarshalIndent(plan.ExecPlan, "", "  "); err != nil {
				response.Write([]byte(err.Error()))
			} else {
				response.Write(b)
			}
			response.Write(([]byte)("\n\n"))
		}
	}
}

func (si *SchemaInfo) handleHTTPQueryStats(response http.ResponseWriter, request *http.Request) {
	keys := si.queries.Keys()
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	qstats := make([]perQueryStats, 0, len(keys))
	for _, v := range keys {
		if plan := si.getQuery(v); plan != nil {
			var pqstats perQueryStats
			pqstats.Query = unicoded(v)
			pqstats.Table = plan.TableName
			pqstats.Plan = plan.PlanId
			pqstats.QueryCount, pqstats.Time, pqstats.RowCount, pqstats.ErrorCount = plan.Stats()
			qstats = append(qstats, pqstats)
		}
	}
	if b, err := json.MarshalIndent(qstats, "", "  "); err != nil {
		response.Write([]byte(err.Error()))
	} else {
		response.Write(b)
	}
}

func (si *SchemaInfo) handleHTTPTableStats(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	tstats := make(map[string]struct{ hits, absent, misses, invalidations int64 })
	var temp, totals struct{ hits, absent, misses, invalidations int64 }
	func() {
		si.mu.Lock()
		defer si.mu.Unlock()
		for k, v := range si.tables {
			if v.CacheType != schema.CACHE_NONE {
				temp.hits, temp.absent, temp.misses, temp.invalidations = v.Stats()
				tstats[k] = temp
				totals.hits += temp.hits
				totals.absent += temp.absent
				totals.misses += temp.misses
				totals.invalidations += temp.invalidations
			}
		}
	}()
	response.Write([]byte("{\n"))
	for k, v := range tstats {
		fmt.Fprintf(response, "\"%s\": {\"Hits\": %v, \"Absent\": %v, \"Misses\": %v, \"Invalidations\": %v},\n", k, v.hits, v.absent, v.misses, v.invalidations)
	}
	fmt.Fprintf(response, "\"Totals\": {\"Hits\": %v, \"Absent\": %v, \"Misses\": %v, \"Invalidations\": %v}\n", totals.hits, totals.absent, totals.misses, totals.invalidations)
	response.Write([]byte("}\n"))
}

func (si *SchemaInfo) handleHTTPSchema(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	tables := si.GetSchema()
	b, err := json.MarshalIndent(tables, "", " ")
	if err != nil {
		response.Write([]byte(err.Error()))
		return
	}
	buf := bytes.NewBuffer(nil)
	json.HTMLEscape(buf, b)
	response.Write(buf.Bytes())
}
