package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mlflow/mlflow-go-backend/pkg/contract"
	"github.com/mlflow/mlflow-go-backend/pkg/entities"
	"github.com/mlflow/mlflow-go-backend/pkg/protos"
	"github.com/mlflow/mlflow-go-backend/pkg/tracking/store/sql/models"
	"github.com/mlflow/mlflow-go-backend/pkg/utils"
)

func convertExperimentIDToInt(id string) (int32, *contract.Error) {
	idInt, err := strconv.ParseInt(id, 10, 32)
	if err != nil {
		return 0, contract.NewErrorWith(
			protos.ErrorCode_INVALID_PARAMETER_VALUE,
			fmt.Sprintf("failed to convert experiment id %q to int", id),
			err,
		)
	}

	return int32(idInt), nil
}

func checkExperimentIsActive(experiment *entities.Experiment) *contract.Error {
	if models.LifecycleStage(experiment.LifecycleStage) != models.LifecycleStageActive {
		return contract.NewError(
			protos.ErrorCode_INVALID_PARAMETER_VALUE,
			fmt.Sprintf(
				"The experiment %q must be in the 'active' state.\n"+
					"Current state is %q.",
				experiment.ExperimentID,
				experiment.LifecycleStage,
			),
		)
	}

	return nil
}

func (s TrackingSQLStore) GetExperiment(ctx context.Context, id string) (*entities.Experiment, *contract.Error) {
	experimentID, err := convertExperimentIDToInt(id)
	if err != nil {
		return nil, err
	}

	experiment := models.Experiment{ID: experimentID}
	if err := s.db.WithContext(ctx).Preload("Tags").First(&experiment).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, contract.NewError(
				protos.ErrorCode_RESOURCE_DOES_NOT_EXIST,
				fmt.Sprintf("No Experiment with id=%d exists", experimentID),
			)
		}

		return nil, contract.NewErrorWith(
			protos.ErrorCode_INTERNAL_ERROR,
			"failed to get experiment",
			err,
		)
	}

	return experiment.ToEntity(), nil
}

func (s TrackingSQLStore) CreateExperiment(
	ctx context.Context,
	name string,
	artifactLocation string,
	tags []*entities.ExperimentTag,
) (string, *contract.Error) {
	experiment := models.Experiment{
		Name:             name,
		Tags:             make([]models.ExperimentTag, len(tags)),
		ArtifactLocation: artifactLocation,
		LifecycleStage:   models.LifecycleStageActive,
		CreationTime:     time.Now().UnixMilli(),
		LastUpdateTime:   time.Now().UnixMilli(),
	}

	for i, tag := range tags {
		experiment.Tags[i] = models.ExperimentTag{
			Key:   tag.Key,
			Value: tag.Value,
		}
	}

	if err := s.db.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		if err := transaction.Create(&experiment).Error; err != nil {
			return fmt.Errorf("failed to insert experiment: %w", err)
		}

		if experiment.ArtifactLocation == "" {
			artifactLocation, err := utils.AppendToURIPath(s.config.DefaultArtifactRoot, strconv.Itoa(int(experiment.ID)))
			if err != nil {
				return fmt.Errorf("failed to join artifact location: %w", err)
			}
			experiment.ArtifactLocation = artifactLocation
			if err := transaction.Model(&experiment).UpdateColumn("artifact_location", artifactLocation).Error; err != nil {
				return fmt.Errorf("failed to update experiment artifact location: %w", err)
			}
		}

		return nil
	}); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return "", contract.NewError(
				protos.ErrorCode_RESOURCE_ALREADY_EXISTS,
				fmt.Sprintf("Experiment(name=%s) already exists.", experiment.Name),
			)
		}

		return "", contract.NewErrorWith(protos.ErrorCode_INTERNAL_ERROR, "failed to create experiment", err)
	}

	return strconv.Itoa(int(experiment.ID)), nil
}

func (s TrackingSQLStore) RenameExperiment(
	ctx context.Context, experimentID, name string,
) *contract.Error {
	if err := s.db.WithContext(ctx).Model(&models.Experiment{}).
		Where("experiment_id = ?", experimentID).
		Updates(&models.Experiment{
			Name:           name,
			LastUpdateTime: time.Now().UnixMilli(),
		}).Error; err != nil {
		return contract.NewErrorWith(protos.ErrorCode_INTERNAL_ERROR, "failed to update experiment", err)
	}

	return nil
}

func (s TrackingSQLStore) DeleteExperiment(ctx context.Context, id string) *contract.Error {
	experimentID, err := convertExperimentIDToInt(id)
	if err != nil {
		return err
	}

	if err := s.db.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		// Update experiment
		uex := transaction.Model(&models.Experiment{}).
			Where("experiment_id = ?", experimentID).
			Updates(&models.Experiment{
				LifecycleStage: models.LifecycleStageDeleted,
				LastUpdateTime: time.Now().UnixMilli(),
			})

		if uex.Error != nil {
			return fmt.Errorf("failed to update experiment (%d) during delete: %w", experimentID, err)
		}

		if uex.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}

		// Update runs
		if err := transaction.Model(&models.Run{}).
			Where("experiment_id = ?", experimentID).
			Updates(&models.Run{
				LifecycleStage: models.LifecycleStageDeleted,
				DeletedTime:    sql.NullInt64{Valid: true, Int64: time.Now().UnixMilli()},
			}).Error; err != nil {
			return fmt.Errorf("failed to update runs during delete: %w", err)
		}

		return nil
	}); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return contract.NewError(
				protos.ErrorCode_RESOURCE_DOES_NOT_EXIST,
				fmt.Sprintf("No Experiment with id=%d exists", experimentID),
			)
		}

		return contract.NewErrorWith(
			protos.ErrorCode_INTERNAL_ERROR,
			"failed to delete experiment",
			err,
		)
	}

	return nil
}

func (s TrackingSQLStore) RestoreExperiment(ctx context.Context, id string) *contract.Error {
	experimentID, err := convertExperimentIDToInt(id)
	if err != nil {
		return err
	}

	if err := s.db.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		// Update experiment
		uex := transaction.Model(&models.Experiment{}).
			Where("experiment_id = ?", experimentID).
			Where("lifecycle_stage = ?", models.LifecycleStageDeleted).
			Updates(&models.Experiment{
				LifecycleStage: models.LifecycleStageActive,
				LastUpdateTime: time.Now().UnixMilli(),
			})

		if uex.Error != nil {
			return fmt.Errorf("failed to update experiment (%d) during delete: %w", experimentID, err)
		}

		if uex.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}

		// Update runs
		if err := transaction.Model(&models.Run{}).
			Where("experiment_id = ?", experimentID).
			Select("DeletedTime", "LifecycleStage").
			Updates(&models.Run{
				LifecycleStage: models.LifecycleStageActive,
				DeletedTime:    sql.NullInt64{},
			}).Error; err != nil {
			return fmt.Errorf("failed to update runs during restore: %w", err)
		}

		return nil
	}); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return contract.NewError(
				protos.ErrorCode_RESOURCE_DOES_NOT_EXIST,
				fmt.Sprintf("No Experiment with id=%d exists", experimentID),
			)
		}

		return contract.NewErrorWith(
			protos.ErrorCode_INTERNAL_ERROR,
			"failed to delete experiment",
			err,
		)
	}

	return nil
}

//nolint:perfsprint
func (s TrackingSQLStore) GetExperimentByName(
	ctx context.Context, name string,
) (*entities.Experiment, *contract.Error) {
	var experiment models.Experiment

	err := s.db.WithContext(ctx).Preload("Tags").Where("name = ?", name).First(&experiment).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, contract.NewError(
				protos.ErrorCode_RESOURCE_DOES_NOT_EXIST,
				fmt.Sprintf("Could not find experiment with name %s", name),
			)
		}

		return nil, contract.NewErrorWith(
			protos.ErrorCode_INTERNAL_ERROR,
			fmt.Sprintf("failed to get experiment by name %s", name),
			err,
		)
	}

	return experiment.ToEntity(), nil
}

func (s TrackingSQLStore) SearchExperiments(
	ctx context.Context,
	experimentViewType protos.ViewType,
	maxResults int64,
	filter string,
	orderBy []string,
	pageToken string,
) ([]*entities.Experiment, string, *contract.Error) {
	query := applyExperimentsLifecycleStagesFilter(s.db.WithContext(ctx), experimentViewType)

	// apply Limit
	query, limit := applyExperimentsLimitFilter(query, maxResults)

	// apply Offset
	query, offset, err := applyExperimentsOffsetFilter(query, pageToken)
	if err != nil {
		return nil, "", err
	}

	// Apply Filter
	query, err = applyExperimentsFilter(s.db, query, filter)
	if err != nil {
		return nil, "", err
	}

	// OrderBy
	query, err = applyExperimentsOrderBy(query, orderBy)
	if err != nil {
		return nil, "", err
	}

	// Actual query
	var experiments []models.Experiment
	if err := query.Preload("Tags").Find(&experiments).Error; err != nil {
		return nil, "", contract.NewErrorWith(
			protos.ErrorCode_INTERNAL_ERROR,
			fmt.Sprintf("failed to get runs %q", err),
			err,
		)
	}

	// encode `nextPageToken` value.
	nextPageToken, err := createExperimentsNextPageToken(experiments, limit, offset)
	if err != nil {
		return nil, "", err
	}

	if len(experiments) > limit {
		experiments = experiments[:limit]
	}

	data := make([]*entities.Experiment, len(experiments))
	for i, experiment := range experiments {
		data[i] = experiment.ToEntity()
	}

	return data, nextPageToken, nil
}

func (s TrackingSQLStore) SetExperimentTag(
	ctx context.Context, experimentID, key, value string,
) *contract.Error {
	experiment, err := s.GetExperiment(ctx, experimentID)
	if err != nil {
		return err
	}

	if err := checkExperimentIsActive(experiment); err != nil {
		return err
	}

	idInt, err := convertExperimentIDToInt(experimentID)
	if err != nil {
		return err
	}

	if err := s.db.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		experimentTag := models.ExperimentTag{
			ExperimentID: idInt,
			Key:          key,
			Value:        value,
		}

		if err := transaction.Clauses(clause.OnConflict{
			UpdateAll: true,
		}).Create(&experimentTag).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return fmt.Errorf("tag already exists for experiment %s: %w", experimentID, err)
			}

			return fmt.Errorf("failed to create experiment tag: %w", err)
		}

		return nil
	}); err != nil {
		return contract.NewErrorWith(
			protos.ErrorCode_INTERNAL_ERROR,
			"failed to set experiment tag",
			err,
		)
	}

	return nil
}
