// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2012 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"context"
	"strings"
	"time"

	"fmt"

	"git.opencp.cn/sde-base/seata-golang/pkg/apis"
	"git.opencp.cn/sde-base/seata-golang/pkg/client/base/exception"
	"git.opencp.cn/sde-base/seata-golang/pkg/client/config"
	"git.opencp.cn/sde-base/seata-golang/pkg/client/rm"
	"git.opencp.cn/sde-base/seata-golang/pkg/util/log"
	"github.com/pkg/errors"
)

type mysqlTx struct {
	mc *mysqlConn
}

func (tx *mysqlTx) Commit() (err error) {
	defer func() {
		if tx.mc != nil {
			tx.mc.ctx = nil
		}
		tx.mc = nil
	}()

	if tx.mc == nil || tx.mc.closed.Load() {
		return ErrInvalidConn
	}
	if tx.mc.xid == "" {
		if tx.mc.ctx != nil {
			branchID, err := tx.register()
			if err != nil {
				rollBackErr := tx.mc.exec("ROLLBACK")
				if rollBackErr != nil {
					log.Error(rollBackErr)
				}
				return err
			}
			tx.mc.ctx.branchID = branchID

			if len(tx.mc.ctx.sqlUndoItemsBuffer) > 0 {
				err = GetUndoLogManager().FlushUndoLogs(tx.mc)
				if err != nil {
					reportErr := tx.report(false)
					if reportErr != nil {
						return reportErr
					}
					return err
				}
				err = tx.mc.exec("COMMIT")
				if err != nil {
					reportErr := tx.report(false)
					if reportErr != nil {
						return reportErr
					}
					return err
				}
			} else {
				err = tx.mc.exec("COMMIT")
				return err
			}
		} else {
			err = tx.mc.exec("COMMIT")
			return err
		}
	} else {
		err = tx.mc.exec(fmt.Sprintf("xa commit '%s'", tx.mc.xid))
		tx.mc.xid = ""
	}
	return
}

func (tx *mysqlTx) Rollback() (err error) {
	defer func() {
		if tx.mc != nil {
			tx.mc.ctx = nil
		}
		tx.mc = nil
	}()

	if tx.mc == nil || tx.mc.closed.Load() {
		return ErrInvalidConn
	}

	if tx.mc.xid == "" {
		err = tx.mc.exec("ROLLBACK")
		if tx.mc.ctx != nil {
			branchID, err := tx.register()
			if err != nil {
				return err
			}
			tx.mc.ctx.branchID = branchID
			tx.report(false)
		}
	} else {
		err = tx.mc.exec(fmt.Sprintf("xa rollback '%s'", tx.mc.xid))
		tx.mc.xid = ""
	}
	return
}

func (tx *mysqlTx) register() (int64, error) {
	var branchID int64
	var err error
	for retryCount := 0; retryCount < config.GetATConfig().LockRetryTimes; retryCount++ {
		branchID, err = rm.GetResourceManager().BranchRegister(context.Background(),
			tx.mc.ctx.xid, tx.mc.cfg.DBName, apis.AT, nil,
			strings.Join(tx.mc.ctx.lockKeys, ";"))
		if err == nil {
			break
		}
		log.Errorf("branch register err: %v", err)
		var tex *exception.TransactionException
		if errors.As(err, &tex) {
			if tex.Code == apis.GlobalTransactionNotExist {
				break
			}
		}
		time.Sleep(config.GetATConfig().LockRetryInterval)
	}
	return branchID, err
}

func (tx *mysqlTx) report(commitDone bool) error {
	retry := config.GetATConfig().LockRetryTimes
	for retry > 0 {
		var err error
		if commitDone {
			err = rm.GetResourceManager().BranchReport(context.Background(),
				tx.mc.ctx.xid, tx.mc.ctx.branchID, apis.AT, apis.PhaseOneDone, nil)
		} else {
			err = rm.GetResourceManager().BranchReport(context.Background(),
				tx.mc.ctx.xid, tx.mc.ctx.branchID, apis.AT, apis.PhaseOneFailed, nil)
		}
		if err != nil {
			log.Errorf("Failed to report [%d/%s] commit done [%t] Retry Countdown: %d",
				tx.mc.ctx.branchID, tx.mc.ctx.xid, commitDone, retry)
		}
		retry = retry - 1
		if retry == 0 {
			return errors.WithMessagef(err, "Failed to report branch status %t", commitDone)
		}
	}
	return nil
}
