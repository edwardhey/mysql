// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2012 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"reflect"

	"git.opencp.cn/sde-base/seata-golang/pkg/client/config"
	"github.com/pingcap/parser"
	"github.com/pingcap/parser/ast"
	_ "github.com/pingcap/parser/test_driver"
)

type mysqlStmt struct {
	mc         *mysqlConn
	id         uint32
	paramCount int
	sql        string
}

func (stmt *mysqlStmt) Close() error {
	if stmt.mc == nil || stmt.mc.closed.Load() {
		// driver.Stmt.Close can be called more than once, thus this function
		// has to be idempotent.
		// See also Issue #450 and golang/go#16019.
		//errLog.Print(ErrInvalidConn)
		return driver.ErrBadConn
	}

	err := stmt.mc.writeCommandPacketUint32(comStmtClose, stmt.id)
	stmt.mc = nil
	return err
}

func (stmt *mysqlStmt) NumInput() int {
	return stmt.paramCount
}

func (stmt *mysqlStmt) ColumnConverter(idx int) driver.ValueConverter {
	return converter{}
}

func (stmt *mysqlStmt) CheckNamedValue(nv *driver.NamedValue) (err error) {
	nv.Value, err = converter{}.ConvertValue(nv.Value)
	return
}

func (stmt *mysqlStmt) Exec(args []driver.Value) (driver.Result, error) {
	if stmt.mc.closed.Load() {
		stmt.mc.log(ErrInvalidConn)
		return nil, driver.ErrBadConn
	}

	if stmt.mc.ctx != nil {
		parser := parser.New()
		act, _ := parser.ParseOneStmt(stmt.sql, "", "")
		deleteStmt, isDelete := act.(*ast.DeleteStmt)
		if isDelete {
			executor := &deleteExecutor{
				mc:          stmt.mc,
				originalSQL: stmt.sql,
				stmt:        deleteStmt,
				args:        args,
			}
			stmt.mc.writeCommandPacketUint32(comStmtClose, stmt.id)
			return executor.Execute()
		}

		insertStmt, isInsert := act.(*ast.InsertStmt)
		if isInsert {
			executor := &insertExecutor{
				mc:          stmt.mc,
				originalSQL: stmt.sql,
				stmt:        insertStmt,
				args:        args,
			}
			stmt.mc.writeCommandPacketUint32(comStmtClose, stmt.id)
			return executor.Execute()
		}

		updateStmt, isUpdate := act.(*ast.UpdateStmt)
		if isUpdate {
			executor := &updateExecutor{
				mc:          stmt.mc,
				originalSQL: stmt.sql,
				stmt:        updateStmt,
				args:        args,
			}
			stmt.mc.writeCommandPacketUint32(comStmtClose, stmt.id)
			return executor.Execute()
		}
	}

	// Send command
	err := stmt.writeExecutePacket(args)
	if err != nil {
		return nil, stmt.mc.markBadConn(err)
	}

	mc := stmt.mc
	handleOk := stmt.mc.clearResult()

	// Read Result
	resLen, err := handleOk.readResultSetHeaderPacket()
	if err != nil {
		return nil, err
	}

	if resLen > 0 {
		// Columns
		if err = mc.readUntilEOF(); err != nil {
			return nil, err
		}

		// Rows
		if err := mc.readUntilEOF(); err != nil {
			return nil, err
		}
	}

	if err := handleOk.discardResults(); err != nil {
		return nil, err
	}

	copied := mc.result
	return &copied, nil
}

func (stmt *mysqlStmt) Query(args []driver.Value) (driver.Rows, error) {
	if stmt.mc.closed.Load() {
		stmt.mc.log(ErrInvalidConn)
		return nil, driver.ErrBadConn
	}

	if stmt.mc.ctx != nil {
		parser := parser.New()
		act, _ := parser.ParseOneStmt(stmt.sql, "", "")
		selectForUpdateStmt, ok := act.(*ast.SelectStmt)
		if ok && selectForUpdateStmt.LockTp == ast.SelectLockForUpdate {
			executor := &selectForUpdateExecutor{
				mc:          stmt.mc,
				originalSQL: stmt.sql,
				stmt:        selectForUpdateStmt,
				args:        args,
			}
			return executor.Execute(config.GetATConfig().LockRetryInterval, config.GetATConfig().LockRetryTimes)
		}
	}
	return stmt.query(args)
}

func (stmt *mysqlStmt) query(args []driver.Value) (*binaryRows, error) {
	if stmt.mc.closed.Load() {
		stmt.mc.log(ErrInvalidConn)
		return nil, driver.ErrBadConn
	}
	// Send command
	err := stmt.writeExecutePacket(args)
	if err != nil {
		return nil, stmt.mc.markBadConn(err)
	}

	mc := stmt.mc

	// Read Result
	handleOk := stmt.mc.clearResult()
	resLen, err := handleOk.readResultSetHeaderPacket()
	if err != nil {
		return nil, err
	}

	rows := new(binaryRows)

	if resLen > 0 {
		rows.mc = mc
		rows.rs.columns, err = mc.readColumns(resLen)
	} else {
		rows.rs.done = true

		switch err := rows.NextResultSet(); err {
		case nil, io.EOF:
			return rows, nil
		default:
			return nil, err
		}
	}

	return rows, err
}

var jsonType = reflect.TypeOf(json.RawMessage{})

type converter struct{}

// ConvertValue mirrors the reference/default converter in database/sql/driver
// with _one_ exception.  We support uint64 with their high bit and the default
// implementation does not.  This function should be kept in sync with
// database/sql/driver defaultConverter.ConvertValue() except for that
// deliberate difference.
func (c converter) ConvertValue(v any) (driver.Value, error) {
	if driver.IsValue(v) {
		return v, nil
	}

	if vr, ok := v.(driver.Valuer); ok {
		sv, err := callValuerValue(vr)
		if err != nil {
			return nil, err
		}
		if driver.IsValue(sv) {
			return sv, nil
		}
		// A value returned from the Valuer interface can be "a type handled by
		// a database driver's NamedValueChecker interface" so we should accept
		// uint64 here as well.
		if u, ok := sv.(uint64); ok {
			return u, nil
		}
		return nil, fmt.Errorf("non-Value type %T returned from Value", sv)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr:
		// indirect pointers
		if rv.IsNil() {
			return nil, nil
		} else {
			return c.ConvertValue(rv.Elem().Interface())
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint(), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.Slice:
		switch t := rv.Type(); {
		case t == jsonType:
			return v, nil
		case t.Elem().Kind() == reflect.Uint8:
			return rv.Bytes(), nil
		default:
			return nil, fmt.Errorf("unsupported type %T, a slice of %s", v, t.Elem().Kind())
		}
	case reflect.String:
		return rv.String(), nil
	}
	return nil, fmt.Errorf("unsupported type %T, a %s", v, rv.Kind())
}

var valuerReflectType = reflect.TypeOf((*driver.Valuer)(nil)).Elem()

// callValuerValue returns vr.Value(), with one exception:
// If vr.Value is an auto-generated method on a pointer type and the
// pointer is nil, it would panic at runtime in the panicwrap
// method. Treat it like nil instead.
//
// This is so people can implement driver.Value on value types and
// still use nil pointers to those types to mean nil/NULL, just like
// string/*string.
//
// This is an exact copy of the same-named unexported function from the
// database/sql package.
func callValuerValue(vr driver.Valuer) (v driver.Value, err error) {
	if rv := reflect.ValueOf(vr); rv.Kind() == reflect.Ptr &&
		rv.IsNil() &&
		rv.Type().Elem().Implements(valuerReflectType) {
		return nil, nil
	}
	return vr.Value()
}
