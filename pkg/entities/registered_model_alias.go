package entities

import (
	"github.com/mlflow/mlflow-go-backend/pkg/protos"
	"github.com/mlflow/mlflow-go-backend/pkg/utils"
)

type RegisteredModelAlias struct {
	Alias   string
	Version string
}

func (t RegisteredModelAlias) ToProto() *protos.RegisteredModelAlias {
	return &protos.RegisteredModelAlias{
		Alias:   utils.PtrTo(t.Alias),
		Version: utils.PtrTo(t.Version),
	}
}
