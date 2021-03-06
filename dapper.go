package dapper

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
)

var (
	ErrNoTableName  = errors.New("dapper: no table name specified")
	ErrNoPrimaryKey = errors.New("dapper: no primary key column specified")
)

// Session represents an interface to a database.
type Session struct {
	db      *sql.DB
	dialect Dialect
	debug   bool
}

// Finder is a type for querying the database.
type finder struct {
	session  *Session
	db       *sql.DB
	sqlQuery string
	param    interface{}
	debug    bool
	includes []string
}

// New creates a Session from a database connection.
func New(db *sql.DB) *Session {
	return &Session{db: db, dialect: MySQL, debug: false}
}

// Dialect allows for specific SQL dialects.
func (s *Session) Dialect(dialect Dialect) *Session {
	if dialect != nil {
		s.dialect = dialect
	} else {
		s.dialect = MySQL
	}
	return s
}

// GetDialect returns the dialect used in this session.
func (s *Session) GetDialect() Dialect {
	return s.dialect
}

// Debug enables or disables output of the SQL statements to the logger.
func (s *Session) Debug(debug bool) *Session {
	s.debug = debug
	return s
}

// Q starts a query in the session's dialect.
func (s *Session) Q(table string) *Query {
	return Q(s.dialect, table)
}

// Find opens up the query interface of a Session.
// Parameters in sql start with a colon and will be substituted by the
// corresponding field in the param object. If there are no substitutions,
// pass nil as param.
func (s *Session) Find(sql string, param interface{}) *finder {
	return &finder{
		session:  s,
		db:       s.db,
		sqlQuery: sql,
		param:    param,
		debug:    s.debug,
		includes: make([]string, 0),
	}
}

// Debug enables or disables output of the SQL statements to the logger.
func (f *finder) Debug(debug bool) *finder {
	f.debug = debug
	return f
}

// Include adds associations to be loaded with the results.
// They need to be marked with
// `dapper:"oneToMany=<table_name>.<foreign_key>"` or
// `dapper:"oneToOne=<table_name>.<foreign_key>"`
// in the table setup.
// The <table_name> can be omitted if it is unambigious.
func (f *finder) Include(associations ...string) *finder {
	f.includes = append(f.includes, associations...)
	return f
}

// ---- Get ------------------------------------------------------------------

// Get loads an entity by its primary key.
//
// Example:
// var out Order
// err := session.Get(1).Do(&out)
func (s *Session) Get(pk interface{}) *getRequest {
	return &getRequest{
		s:        s,
		db:       s.db,
		pk:       pk,
		debug:    s.debug,
		includes: make([]string, 0),
	}
}

// getRequest encapsulates a request for an entity by its primary key
// via the Get method.
type getRequest struct {
	s        *Session
	db       *sql.DB
	pk       interface{}
	debug    bool
	includes []string
}

// Debug enables or disables output of the SQL statements to the logger.
func (r *getRequest) Debug(debug bool) *getRequest {
	r.debug = debug
	return r
}

// Include adds associations to be loaded in addition to the model.
// They need to be marked with
// `dapper:"oneToMany=<table_name>.<foreign_key>"` or
// `dapper:"oneToOne=<table_name>.<foreign_key>"`
// in the table setup.
func (r *getRequest) Include(associations ...string) *getRequest {
	r.includes = append(r.includes, associations...)
	return r
}

// Do executes the getRequest and returns the loaded entity in the result.
// If everything is okay, nil is returned. If the entity cannot be found,
// sql.ErrNoRows is returned.
func (r *getRequest) Do(result interface{}) error {
	// Get information about result
	resultValue := reflect.ValueOf(result)
	if resultValue.Kind() != reflect.Ptr {
		return errors.New("result must be a pointer to a struct")
	}

	indirectValue := reflect.Indirect(resultValue)
	gotype := indirectValue.Type()

	resultInfo, err := AddType(gotype)
	if err != nil {
		return err
	}

	tableName := resultInfo.TableName
	pkCol, found := resultInfo.GetPrimaryKey()
	if !found {
		return ErrNoPrimaryKey
	}

	sqlQuery := r.s.Q(tableName).Where().Eq(pkCol.ColumnName, r.pk).Sql()

	if r.debug {
		log.Println(sqlQuery)
	}

	// We use Query instead of QueryRow, because row does not contain
	// Column information
	rows, err := r.db.Query(sqlQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Scan fills all fields in dst here
	var placeholder interface{}
	if rows.Next() {
		resultFields := make([]interface{}, 0)
		dbColumnNames, err := rows.Columns()
		if err != nil {
			return err
		}
		for _, dbColName := range dbColumnNames {
			fi, found := resultInfo.ColumnInfos[dbColName]
			if found {
				field := resultValue.Elem().FieldByName(fi.FieldName)
				resultFields = append(resultFields, field.Addr().Interface())
			} else {
				// Ignore missing columns
				resultFields = append(resultFields, &placeholder)
				/*
					return errors.New(
						fmt.Sprintf("type %s: found no corresponding mapping "+
							"for column %s in result", gotype, dbColName))
				*/
			}
		}

		// Scan results
		err = rows.Scan(resultFields...)
		if err != nil {
			return err
		}

		// Load associations
		err = r.s.loadAssociations(gotype, resultInfo, resultValue, r.includes)
		if err != nil {
			return err
		}

	} else {
		// If there's no row, we should return sql.ErrNoRows
		return sql.ErrNoRows
	}

	return nil
}

// ---- Single ---------------------------------------------------------------

// Single returns the first result of the SQL query in result.
//
// If no rows are found, sql.ErrNoRows is returned (see sql.QueryRow).
//
// Example:
// param := UserByIdQuery{Id: 42}
// var result User{}
// err := session.Find("select * from users where id=:Id", param).Single(&result)
func (q *finder) Single(result interface{}) error {
	// Get information about result
	resultValue := reflect.ValueOf(result)
	if resultValue.Kind() != reflect.Ptr {
		return errors.New("result must be a pointer to a struct")
	}

	indirectValue := reflect.Indirect(resultValue)
	gotype := indirectValue.Type()

	resultInfo, err := AddType(gotype)
	if err != nil {
		return err
	}

	// Get information about param
	sqlQuery := q.sqlQuery
	if q.param != nil {
		paramValue := reflect.ValueOf(q.param)
		if paramValue.Kind() == reflect.Ptr {
			paramValue = paramValue.Elem()
		}
		paramInfo, err := AddType(paramValue.Type())
		if err != nil {
			return err
		}

		// Substitute parameters in SQL statement
		for paramName, fi := range paramInfo.FieldInfos {
			if fi.IsTransient {
				continue
			}
			// Get value of field in param
			field := paramValue.FieldByName(paramName)
			value := field.Interface()
			quoted := Quote(q.session.dialect, value)
			sqlQuery = strings.Replace(sqlQuery, ":"+paramName, quoted, -1)
		}
	}

	if q.debug {
		log.Println(sqlQuery)
	}

	// We use Query instead of QueryRow, because row does not contain Column information
	rows, err := q.db.Query(sqlQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Scan fills all fields in dst here
	var placeholder interface{}
	if rows.Next() {
		resultFields := make([]interface{}, 0)
		dbColumnNames, err := rows.Columns()
		if err != nil {
			return err
		}
		for _, dbColName := range dbColumnNames {
			fi, found := resultInfo.ColumnInfos[dbColName]
			if found {
				field := resultValue.Elem().FieldByName(fi.FieldName)
				resultFields = append(resultFields, field.Addr().Interface())
			} else {
				// Ignore missing columns
				resultFields = append(resultFields, &placeholder)
				/*
					return errors.New(
						fmt.Sprintf("type %s: found no corresponding mapping "+
							"for column %s in result", gotype, dbColName))
				*/
			}
		}

		// Scan results
		err = rows.Scan(resultFields...)
		if err != nil {
			return err
		}

		// Load associations
		err = q.session.loadAssociations(gotype, resultInfo, resultValue, q.includes)
		if err != nil {
			return err
		}
	} else {
		// If there's no row, we should return sql.ErrNoRows
		return sql.ErrNoRows
	}

	return nil
}

// ---- All -----------------------------------------------------------------

// All returns a slice of results of the SQL query in result.
// The result parameter must be a pointer to a slice of query results.
// If no rows are found, sql.ErrNoRows is returned.
//
// Example:
// param := UserByCompanyQuery{CompanyId: 42}
// var results []UserByCompanyQuery
// err := session.Find("select * from users "+
//     "where company_id=:CompanyId "+
//     "order by email limit 10", param).All(&results)
func (q *finder) All(result interface{}) error {
	resultv := reflect.ValueOf(result)
	if resultv.Kind() != reflect.Ptr || resultv.Elem().Kind() != reflect.Slice {
		return errors.New("result must be a pointer to a slice")
	}

	slicev := resultv.Elem()
	slicev = slicev.Slice(0, slicev.Cap())
	elemt := slicev.Type().Elem()

	// We accept both slices of structs or slices of pointers to structs
	elemIsPtr := elemt.Kind() == reflect.Ptr

	gotype := elemt
	if elemIsPtr {
		gotype = elemt.Elem()
	}

	resultInfo, err := AddType(gotype)
	if err != nil {
		return err
	}

	// Get information about param
	sqlQuery := q.sqlQuery
	if q.param != nil {
		paramValue := reflect.ValueOf(q.param)
		if paramValue.Kind() == reflect.Ptr {
			paramValue = paramValue.Elem()
		}
		paramInfo, err := AddType(paramValue.Type())
		if err != nil {
			return err
		}

		// Substitute parameters in SQL statement
		for paramName, fi := range paramInfo.FieldInfos {
			if fi.IsTransient {
				continue
			}
			// Get value of field in param
			field := paramValue.FieldByName(paramName)
			value := field.Interface()
			quoted := Quote(q.session.dialect, value)
			sqlQuery = strings.Replace(sqlQuery, ":"+paramName, quoted, -1)
		}
	}

	if q.debug {
		log.Println(sqlQuery)
	}

	rows, err := q.db.Query(sqlQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	i := 0
	var placeholder interface{}
	for rows.Next() {
		// Prepare destination fields for Scan
		singleResult := reflect.New(gotype)

		resultFields := make([]interface{}, 0)
		dbColumnNames, err := rows.Columns()
		if err != nil {
			return err
		}
		for _, dbColName := range dbColumnNames {
			fi, found := resultInfo.ColumnInfos[dbColName]
			if found {
				field := singleResult.Elem().FieldByName(fi.FieldName)
				resultFields = append(resultFields, field.Addr().Interface())
			} else {
				// Ignore missing columns
				resultFields = append(resultFields, &placeholder)
				/*
					return nil, errors.New(
						fmt.Sprintf("type %s: found no corresponding mapping "+
							"for column %s in result", gotype, dbColName))
					//*/
			}
		}

		// Scan fills all fields in singleResult here
		err = rows.Scan(resultFields...)
		if err != nil {
			return err
		}

		// Add resultFields to slice
		if elemIsPtr {
			slicev = reflect.Append(slicev, singleResult.Elem().Addr())
		} else {
			slicev = reflect.Append(slicev, singleResult.Elem())
		}

		i++
	}

	resultv.Elem().Set(slicev.Slice(0, i))

	// -- Load associations ---

	if len(q.includes) > 0 {
		// Load associations by creating a IN query on the child tables
		type QueryByIds struct {
			Query      *Query
			Includes   []string
			IdMap      map[interface{}]bool
			Ids        []interface{}
			ColumnName string
			//Typ        reflect.Type
			TypeInfo  *typeInfo
			OneToOne  *oneToOneInfo
			OneToMany *oneToManyInfo
			Records   []reflect.Value
		}
		oneToOneQueries := make(map[string]QueryByIds)
		oneToManyQueries := make(map[string]QueryByIds)

		// Loop through all elements of the resultset and collect
		// the table name, column name, and ids of the entities
		// to load.
		for k := 0; k < i; k++ {
			assocNames, assocNamesNextLevel := split(q.includes, ".")

			// Gather information about a single entity
			recordv := resultv.Elem().Index(k)
			ti, err := AddType(recordv.Elem().Type())
			if err != nil {
				return err
			}

			// Get its primary key
			pk, found := ti.GetPrimaryKey()
			if !found {
				return ErrNoPrimaryKey
			}
			primaryKey := recordv.Elem().FieldByName(pk.FieldName).Interface()

			// OneToOne
			for _, assocName := range assocNames {
				assoc, found := ti.OneToOneInfos[assocName]
				if !found {
					continue
				}

				// Retrieve table name and column name of the references table
				assocTableName, err := assoc.GetTableName()
				if err != nil {
					return err
				}
				assocColumnName, err := assoc.GetColumnName()
				if err != nil {
					return err
				}

				// Add oneToOne information so that they can be loaded later
				targetField := recordv.Elem().FieldByName(assoc.FieldName)
				if targetField.Kind() != reflect.Ptr {
					return errors.New("dapper: a field marked with oneToOne must be a pointer")
				}
				idQ, found := oneToOneQueries[assocTableName]
				if !found {
					idQ = QueryByIds{
						Query:      q.session.Q(assocTableName),
						Includes:   assocNamesNextLevel,
						IdMap:      make(map[interface{}]bool),
						Ids:        make([]interface{}, 0),
						ColumnName: assocColumnName,
						TypeInfo:   ti,
						OneToOne:   assoc,
						Records:    make([]reflect.Value, 0),
					}
				}
				fk := recordv.Elem().FieldByName(assoc.ForeignKeyField).Interface()
				if _, idFound := idQ.IdMap[fk]; !idFound {
					idQ.IdMap[fk] = true
					idQ.Ids = append(idQ.Ids, fk)
				}
				idQ.Records = append(idQ.Records, recordv)
				oneToOneQueries[assocTableName] = idQ
			}

			// OneToMany
			for _, assocName := range assocNames {
				assoc, found := ti.OneToManyInfos[assocName]
				if !found {
					continue
				}

				// Retrieve table name and column name of the references table
				assocTableName, err := assoc.GetTableName()
				if err != nil {
					return err
				}
				assocColumnName, err := assoc.GetColumnName()
				if err != nil {
					return err
				}

				// Add oneToMany information so that they can be loaded later
				idQ, found := oneToManyQueries[assocTableName]
				if !found {
					idQ = QueryByIds{
						Query:      q.session.Q(assocTableName),
						Includes:   assocNamesNextLevel,
						IdMap:      make(map[interface{}]bool),
						Ids:        make([]interface{}, 0),
						ColumnName: assocColumnName,
						TypeInfo:   ti,
						OneToMany:  assoc,
						Records:    make([]reflect.Value, 0),
					}
				}
				if _, idFound := idQ.IdMap[primaryKey]; !idFound {
					idQ.IdMap[primaryKey] = true
					idQ.Ids = append(idQ.Ids, primaryKey)
				}
				idQ.Records = append(idQ.Records, recordv)
				oneToManyQueries[assocTableName] = idQ
			}
		}

		// Now all entities to load are gathered and we'll trigger SQL queries
		// TODO slice queries up into batches of limited size?!
		for _, idQ := range oneToManyQueries {
			query := idQ.Query.Where().In(idQ.ColumnName, idQ.Ids)

			// Load all children
			childrenv := reflect.New(idQ.OneToMany.SliceType)
			children := childrenv.Interface()
			err := q.session.Find(query.Sql(), nil).Include(idQ.Includes...).All(children)
			if err != nil {
				return err
			}

			// Iterate through children, find the parent, and assign the children
			for _, parentv := range idQ.Records {
				parentIdFieldInfo, _ := idQ.TypeInfo.GetPrimaryKey()
				parentIdField := parentv.Elem().FieldByName(parentIdFieldInfo.FieldName)
				var parentId interface{}
				if parentIdField.Kind() != reflect.Ptr {
					parentId = parentIdField.Interface()
				} else if parentIdField.Elem().IsValid() {
					parentId = parentIdField.Elem().Interface()
				}

				// Create a slice for the children
				itemsv := reflect.MakeSlice(reflect.SliceOf(idQ.OneToMany.ElemType), 0, 0) // reflect.SliceOf(idQ.Typ)

				// Iterate through all children in the sub-query
				for k := 0; k < childrenv.Elem().Len(); k++ {
					childv := childrenv.Elem().Index(k)

					fkInResult := childv.Elem().FieldByName(idQ.OneToMany.ForeignKeyField)
					var fk interface{}
					if fkInResult.Kind() != reflect.Ptr {
						fk = fkInResult.Interface()
					} else if fkInResult.Elem().IsValid() {
						fk = fkInResult.Elem().Interface()
					}

					if parentId == fk {
						// we have a matching result in the sub-query
						itemsv = reflect.Append(itemsv, childv.Elem().Addr())
					}
				}

				targetField := parentv.Elem().FieldByName(idQ.OneToMany.FieldName)
				targetField.Set(itemsv)
			}
		}

		// One-to-One queries
		for _, idQ := range oneToOneQueries {
			query := idQ.Query.Where().In(idQ.ColumnName, idQ.Ids)

			// results will contain all the child records
			childrenv := reflect.New(reflect.SliceOf(idQ.OneToOne.TargetType))
			children := childrenv.Interface()
			err := q.session.Find(query.Sql(), nil).Include(idQ.Includes...).All(children)
			if err != nil {
				return err
			}

			// Iterate through entities and assign the matching child
			for k := 0; k < childrenv.Elem().Len(); k++ {
				childv := childrenv.Elem().Index(k)

				childIdFieldInfo, _ := idQ.TypeInfo.GetPrimaryKey()
				childIdField := childv.Elem().FieldByName(childIdFieldInfo.FieldName)
				var childId interface{}
				if childIdField.Kind() != reflect.Ptr {
					childId = childIdField.Interface()
				} else if childIdField.Elem().IsValid() {
					childId = childIdField.Elem().Interface()
				}

				for _, parentv := range idQ.Records {
					parentIdField := parentv.Elem().FieldByName(idQ.OneToOne.ForeignKeyField)
					var parentId interface{}
					if parentIdField.Kind() != reflect.Ptr {
						parentId = parentIdField.Interface()
					} else if parentIdField.Elem().IsValid() {
						parentId = parentIdField.Elem().Interface()
					}

					if childId == parentId {
						// Got a match
						targetField := parentv.Elem().FieldByName(idQ.OneToOne.FieldName)
						targetField.Set(childv.Elem().Addr())
					}
				}
			}
		}
	}

	// -- end: Load associations ---

	return nil
}

// ---- Scalar --------------------------------------------------------------

// Scalar runs the finder query and returns the value of the first column
// of the first row. This is useful for queries such as counting.
//
// The result parameter must be a pointer to a matching value.
// If no rows are found, sql.ErrNoRows is returned.
//
// Example:
// param := UserByIdQuery{Id: 42}
// var count int64
// err := session.Find("select count(*) from users where id=:Id", param).Scalar(&count)
func (q *finder) Scalar(result interface{}) error {
	resultv := reflect.ValueOf(result)
	if resultv.Kind() != reflect.Ptr {
		return errors.New("result must be a pointer")
	}

	sqlQuery := q.sqlQuery

	// Get information about param
	if q.param != nil {
		paramValue := reflect.ValueOf(q.param)
		if paramValue.Kind() == reflect.Ptr {
			paramValue = paramValue.Elem()
		}
		paramInfo, err := AddType(paramValue.Type())
		if err != nil {
			return err
		}

		// Substitute parameters in SQL statement
		for paramName, fi := range paramInfo.FieldInfos {
			if fi.IsTransient {
				continue
			}
			// Get value of field in param
			field := paramValue.FieldByName(paramName)
			value := field.Interface()
			quoted := Quote(q.session.dialect, value)
			sqlQuery = strings.Replace(sqlQuery, ":"+paramName, quoted, -1)
		}
	}

	if q.debug {
		log.Println(sqlQuery)
	}

	row := q.db.QueryRow(sqlQuery)

	elemt := resultv.Type().Elem()
	value := reflect.New(elemt)
	err := row.Scan(value.Interface())
	if err != nil {
		return err
	}

	resultv.Elem().Set(value.Elem())

	return nil
}

// ---- Count ---------------------------------------------------------------

// Count returns the count of the query as an int64.
// If the result is not an int64, it returns ErrWrongType.
//
// Example:
// count, err := session.Count("select count(*) from users", nil)
func (s *Session) Count(sqlQuery string, param interface{}) (int64, error) {
	var count int64
	err := s.Find(sqlQuery, param).Scalar(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// ---- Insert --------------------------------------------------------------

// Insert adds the entity to the database.
func (s *Session) Insert(entity interface{}) error {
	return s.insert(entity, nil)
}

// InsertTx adds the entity to the database.
func (s *Session) InsertTx(tx *sql.Tx, entity interface{}) error {
	return s.insert(entity, tx)
}

// Insert adds the entity to the database.
func (s *Session) insert(entity interface{}, tx *sql.Tx) error {
	// Get information about the entity
	entityv := reflect.ValueOf(entity)
	if entityv.Kind() != reflect.Ptr {
		return errors.New("entity must be a pointer to a struct")
	}

	indirectValue := reflect.Indirect(entityv)
	gotype := indirectValue.Type()

	ti, err := AddType(gotype)
	if err != nil {
		return err
	}

	// Generate SQL query for insert
	sql, err := s.generateInsertSql(ti, entity)
	if err != nil {
		return err
	}

	if s.debug {
		log.Println(sql)
	}

	// Set last insert id if the type has an autoincrement column
	if autoIncrField, hasAutoIncrField := ti.GetAutoIncrement(); hasAutoIncrField {
		// We have an auto_increment field which we'll fill via
		// AUTO_INCREMENT (MySQL), AUTOINCREMENT (Sqlite3), or a sequence (psql)
		var newId int64
		if s.dialect.SupportsLastInsertId() {
			// We get the newId later via LastInsertId()
			res, err := s.exec(tx, sql)
			if err != nil {
				return err
			}
			if newId, err = res.LastInsertId(); err != nil {
				return err
			}
		} else {
			if tx != nil {
				// Query and get RETURNING value in a transaction
				if err := tx.QueryRow(sql).Scan(&newId); err != nil {
					return err
				}
			} else {
				// Query and get RETURNING value without a transaction
				if err := s.db.QueryRow(sql).Scan(&newId); err != nil {
					return err
				}
			}
		}

		// Set autoincrement column to newly generated Id
		field := entityv.Elem().FieldByName(autoIncrField.FieldName)
		field.Set(reflect.ValueOf(newId))
	} else {
		// We don't have to care about auto-increment
		if _, err = s.exec(tx, sql); err != nil {
			return err
		}
	}

	return nil
}

func (s *Session) exec(tx *sql.Tx, sql string) (sql.Result, error) {
	if tx == nil {
		return s.db.Exec(sql)
	}
	return tx.Exec(sql)
}

func (s *Session) generateInsertSql(ti *typeInfo, entity interface{}) (string, error) {
	if ti.TableName == "" {
		return "", ErrNoTableName
	}

	entityv := reflect.ValueOf(entity)

	cnames := make([]string, 0)
	cvals := make([]string, 0)

	var autoIncrField *fieldInfo

	for _, cname := range ti.ColumnNames {
		if fi, found := ti.ColumnInfos[cname]; found {
			if !fi.IsAutoIncrement || fi.IsTransient {
				cnames = append(cnames, s.dialect.EscapeColumnName(cname))

				field := entityv.Elem().FieldByName(fi.FieldName)
				value := field.Interface()
				quoted := Quote(s.dialect, value)
				cvals = append(cvals, quoted)
			} else if fi.IsAutoIncrement {
				autoIncrField = fi
			}
		}
	}

	var sql bytes.Buffer
	sql.WriteString(fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		s.dialect.EscapeTableName(ti.TableName),
		strings.Join(cnames, ", "),
		strings.Join(cvals, ", ")))

	if !s.dialect.SupportsLastInsertId() {
		sql.WriteString(fmt.Sprintf(" RETURNING %s",
			s.dialect.EscapeColumnName(autoIncrField.ColumnName)))
	}

	return sql.String(), nil
}

// ---- Update --------------------------------------------------------------

// Update changes an already existing entity in the database.
func (s *Session) Update(entity interface{}) error {
	return s.update(entity, nil)
}

// UpdateTx changes an already existing entity in the database, but runs
// in a transaction.
func (s *Session) UpdateTx(tx *sql.Tx, entity interface{}) error {
	return s.update(entity, tx)
}

// Update changes an already existing entity in the database.
func (s *Session) update(entity interface{}, tx *sql.Tx) error {
	// Get information about the entity
	entityv := reflect.ValueOf(entity)
	entityIsPtr := entityv.Kind() == reflect.Ptr

	gotype := entityv.Type()
	if entityIsPtr {
		gotype = entityv.Type().Elem()
	}

	ti, err := AddType(gotype)
	if err != nil {
		return err
	}

	// Generate SQL query for update
	sql, err := s.generateUpdateSql(ti, entity)
	if err != nil {
		return err
	}

	if s.debug {
		log.Println(sql)
	}

	if tx == nil {
		// Execute SQL query and return its result
		_, err = s.db.Exec(sql)
		if err != nil {
			return err
		}
	} else {
		// Execute SQL query and return its result
		_, err = tx.Exec(sql)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Session) generateUpdateSql(ti *typeInfo, entity interface{}) (string, error) {
	if ti.TableName == "" {
		return "", ErrNoTableName
	}

	entityv := reflect.ValueOf(entity)
	if entityv.Kind() == reflect.Ptr {
		entityv = entityv.Elem()
	}

	pk, found := ti.GetPrimaryKey()
	if !found {
		return "", ErrNoPrimaryKey
	}
	field := entityv.FieldByName(pk.FieldName)
	pkval := field.Interface()

	pairs := make([]string, 0)

	for _, cname := range ti.ColumnNames {
		if fi, found := ti.ColumnInfos[cname]; found {
			if !fi.IsPrimaryKey || fi.IsTransient {
				field = entityv.FieldByName(fi.FieldName)
				value := field.Interface()
				quoted := Quote(s.dialect, value)
				pair := fmt.Sprintf("%s=%s", s.dialect.EscapeColumnName(cname), quoted)
				pairs = append(pairs, pair)
			}
		}
	}

	return fmt.Sprintf("UPDATE %s SET %s WHERE %s=%s",
		s.dialect.EscapeTableName(ti.TableName),
		strings.Join(pairs, ", "),
		s.dialect.EscapeColumnName(pk.ColumnName),
		Quote(s.dialect, pkval)), nil
}

// ---- Delete --------------------------------------------------------------

// Delete removes the entity from the database.
func (s *Session) Delete(entity interface{}) error {
	return s.delete(entity, nil)
}

// DeleteTx removes the entity from the database, but runs in a transaction.
func (s *Session) DeleteTx(tx *sql.Tx, entity interface{}) error {
	return s.delete(entity, tx)
}

// Delete removes the entity from the database.
func (s *Session) delete(entity interface{}, tx *sql.Tx) error {
	// Get information about the entity
	entityv := reflect.ValueOf(entity)
	entityIsPtr := entityv.Kind() == reflect.Ptr

	gotype := entityv.Type()
	if entityIsPtr {
		gotype = entityv.Type().Elem()
	}

	ti, err := AddType(gotype)
	if err != nil {
		return err
	}

	// Generate SQL query for delete
	sql, err := s.generateDeleteSql(ti, entity)
	if err != nil {
		return err
	}

	if s.debug {
		log.Println(sql)
	}

	if tx == nil {
		// Execute SQL query and return its result
		_, err = s.db.Exec(sql)
		if err != nil {
			return err
		}
	} else {
		// Execute SQL query n transaction and return its result
		_, err = tx.Exec(sql)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Session) generateDeleteSql(ti *typeInfo, entity interface{}) (string, error) {
	if ti.TableName == "" {
		return "", ErrNoTableName
	}

	entityv := reflect.ValueOf(entity)
	if entityv.Kind() == reflect.Ptr {
		entityv = entityv.Elem()
	}

	pk, found := ti.GetPrimaryKey()
	if !found {
		return "", ErrNoPrimaryKey
	}
	field := entityv.FieldByName(pk.FieldName)
	pkval := field.Interface()

	return fmt.Sprintf("DELETE FROM %s WHERE %s=%s",
		s.dialect.EscapeTableName(ti.TableName),
		s.dialect.EscapeColumnName(pk.ColumnName),
		Quote(s.dialect, pkval)), nil
}

// ---- Load associations ----------------------------------------------------

// split takes a slice and splits it on sep and returns both parts.
// It makes sure that duplicates on both parts are ignored.
// Example:
//     []string{"Order", "Order.Items", "Order.Items.Images"}
///    => []string{"Order"}, []string{"Items", "Items.Images"}
func split(includes []string, sep string) ([]string, []string) {
	current := make([]string, 0)
	currentDups := make(map[string]bool)
	remaining := make([]string, 0)
	remainingDups := make(map[string]bool)
	if len(includes) == 0 {
		return current, remaining
	}
	for _, include := range includes {
		str := strings.SplitN(include, sep, 2)
		if len(str) > 0 {
			if _, found := currentDups[str[0]]; !found {
				current = append(current, str[0])
				currentDups[str[0]] = true
			}
		}
		if len(str) > 1 {
			if _, found := remainingDups[str[0]]; !found {
				remaining = append(remaining, str[1])
				remainingDups[str[1]] = true
			}
		}
	}
	return current, remaining
}

func (s *Session) loadAssociations(gotype reflect.Type, resultInfo *typeInfo, resultValue reflect.Value, includes []string) error {
	if len(includes) == 0 {
		return nil
	}

	// Includes can be a dot-separated list of association names.
	// In such a case, associations are loaded recursively.
	assocNames, assocNamesNextLevel := split(includes, ".")

	// Get primary key value
	pk, found := resultInfo.GetPrimaryKey()
	if !found {
		return ErrNoPrimaryKey
	}
	primaryKey := resultValue.Elem().FieldByName(pk.FieldName).Interface()

	// Load 1:1 associations
	for _, assocName := range assocNames {
		assoc, found := resultInfo.OneToOneInfos[assocName]
		if !found {
			continue
		}

		// Retrieve table name and column name of the references table
		assocTableName, err := assoc.GetTableName()
		if err != nil {
			return err
		}
		assocColumnName, err := assoc.GetColumnName()
		if err != nil {
			return err
		}

		// Field where results are to be stored
		targetField := resultValue.Elem().FieldByName(assoc.FieldName)

		// Load oneToOne association
		if targetField.Kind() != reflect.Ptr {
			return errors.New("dapper: a field marked with oneToOne must be a pointer")
		}

		// oneToOne=<table>.<column>.<field>
		fkField := resultValue.Elem().FieldByName(assoc.ForeignKeyField)
		if !fkField.IsValid() {
			return fmt.Errorf("dapper: field %s.%s has a oneToOne association with field %s which is invalid", gotype.String(), assoc.FieldName, assoc.ForeignKeyField)
		}
		if fkField.Kind() == reflect.Ptr && fkField.IsNil() {
			// No need to load
			continue
		}
		fk := fkField.Interface()
		fkTableName := assocTableName
		fkColName := assocColumnName

		subQuery := s.Q(fkTableName).Where().Eq(fkColName, fk).Sql()

		result := reflect.New(targetField.Type().Elem())
		targetField.Set(result)
		err = s.Find(subQuery, nil).Include(assocNamesNextLevel...).Single(targetField.Interface())
		if err != nil {
			return err
		}
	}

	// Load 1:n associations
	// TODO(oe) slice into batches of limited size?!
	for _, assocName := range assocNames {
		assoc, found := resultInfo.OneToManyInfos[assocName]
		if !found {
			continue
		}

		// Retrieve table name and column name of the references table
		assocTableName, err := assoc.GetTableName()
		if err != nil {
			return err
		}
		assocColumnName, err := assoc.GetColumnName()
		if err != nil {
			return err
		}

		// Field where results are to be stored
		targetField := resultValue.Elem().FieldByName(assoc.FieldName)

		// Load oneToMany association
		fkTableName := assocTableName
		fkColName := assocColumnName
		subQuery := s.Q(fkTableName).Where().Eq(fkColName, primaryKey).Sql()

		subResults := targetField.Addr().Interface()
		err = s.Find(subQuery, nil).Include(assocNamesNextLevel...).All(subResults)
		if err != nil {
			return err
		}
	}

	return nil
}

// ---- Exec --------------------------------------------------------------

// Exec executes an SQL statement and parameters.
// It can be used in the same sense as sql.Exec, however the statement
// is logged if debugging is enabled.
func (s *Session) Exec(query string, args ...interface{}) (sql.Result, error) {
	if s.debug {
		log.Printf("%s (%v)", query, args)
	}
	return s.db.Exec(query, args...)
}

// ExecTx executes an SQL statement and parameters in a transaction.
// It can be used in the same sense as sql.Exec, however the statement
// is logged if debugging is enabled.
func (s *Session) ExecTx(tx *sql.Tx, query string, args ...interface{}) (sql.Result, error) {
	if s.debug {
		log.Printf("%s (%v)", query, args)
	}
	return tx.Exec(query, args...)
}

// ---- Transactions ------------------------------------------------------

// Begin starts a new transaction and can be used as a placeholder to sql.Begin.
// However, the statement is logged if debugging is enabled.
func (s *Session) Begin() (*sql.Tx, error) {
	if s.debug {
		log.Println("BEGIN TRANSACTION")
	}
	return s.db.Begin()
}

// Rollback can be used as a placeholder to tx.Rollback.
// However, the statement is logged if debugging is enabled.
func (s *Session) Rollback(tx *sql.Tx) error {
	if s.debug {
		log.Println("ROLLBACK")
	}
	return tx.Rollback()
}

// Commit can be used as a placeholder to tx.Commit.
// However, the statement is logged if debugging is enabled.
func (s *Session) Commit(tx *sql.Tx) error {
	if s.debug {
		log.Println("COMMIT")
	}
	return tx.Commit()
}
