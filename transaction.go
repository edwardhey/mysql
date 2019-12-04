// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2012 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import "fmt"

type mysqlTx struct {
	mc *mysqlConn
}

func (tx *mysqlTx) Commit() (err error) {
	if tx.mc == nil || tx.mc.closed.IsSet() {
		return ErrInvalidConn
	}
	if tx.mc.xid == "" {
		err = tx.mc.exec("COMMIT")
	} else {
		err = tx.mc.exec(fmt.Sprintf("xa commit '%s'", tx.mc.xid))
	}
	tx.mc = nil
	return
}

func (tx *mysqlTx) Rollback() (err error) {
	if tx.mc == nil || tx.mc.closed.IsSet() {
		return ErrInvalidConn
	}
	if tx.mc.xid == "" {
		err = tx.mc.exec("ROLLBACK")
	} else {
		err = tx.mc.exec(fmt.Sprintf("xa rollback '%s'", tx.mc.xid))
	}
	tx.mc = nil
	return
}
