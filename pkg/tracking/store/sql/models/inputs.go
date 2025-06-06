package models

import (
	"github.com/mlflow/mlflow-go-backend/pkg/entities"
)

const (
	SourceTypeDataset          = "DATASET"
	DestinationTypeRun         = "RUN"
	DestinationTypeModelInput  = "MODEL_INPUT"
	DestinationTypeModelOutput = "MODEL_OUTPUT"
)

// Input mapped from table <inputs>.
type Input struct {
	ID              string     `gorm:"column:input_uuid;not null"`
	SourceType      string     `gorm:"column:source_type;primaryKey"`
	SourceID        string     `gorm:"column:source_id;primaryKey"`
	DestinationType string     `gorm:"column:destination_type;primaryKey"`
	DestinationID   string     `gorm:"column:destination_id;primaryKey"`
	Tags            []InputTag `gorm:"foreignKey:InputID;references:ID"`
	Dataset         Dataset    `gorm:"foreignKey:ID;references:SourceID"`
}

func (i Input) TableName() string {
	return "inputs"
}

func (i Input) DatasetToEntity() *entities.DatasetInput {
	tags := make([]*entities.InputTag, 0, len(i.Tags))
	for _, tag := range i.Tags {
		tags = append(tags, tag.ToEntity())
	}

	return &entities.DatasetInput{
		Tags:    tags,
		Dataset: i.Dataset.ToEntity(),
	}
}

func (i Input) ModelInputToEntity() *entities.ModelInput {
	return &entities.ModelInput{
		ModelID: i.DestinationID,
	}
}

func NewInputFromEntity(
	id, sourceID, destinationID string,
) *Input {
	return &Input{
		ID:              id,
		SourceType:      SourceTypeDataset,
		SourceID:        sourceID,
		DestinationType: DestinationTypeRun,
		DestinationID:   destinationID,
	}
}
