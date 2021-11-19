package model

import (
	"gitlab.goiot.net/sde-base/seata-golang/pkg/apis"
)

// Resource used to manage transaction resource
type Resource interface {
	GetResourceID() string

	GetBranchType() apis.BranchSession_BranchType
}
