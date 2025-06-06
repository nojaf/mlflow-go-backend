package sql

import (
	"context"
	"errors"
	"fmt"
	"math"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/mlflow/mlflow-go-backend/pkg/contract"
	"github.com/mlflow/mlflow-go-backend/pkg/entities"
	"github.com/mlflow/mlflow-go-backend/pkg/protos"
	"github.com/mlflow/mlflow-go-backend/pkg/tracking/store/sql/models"
)

const metricsBatchSize = 500

func getDistinctMetricKeys(metrics []models.Metric) []string {
	metricKeysMap := make(map[string]any)
	for _, m := range metrics {
		metricKeysMap[m.Key] = nil
	}

	metricKeys := make([]string, 0, len(metricKeysMap))
	for key := range metricKeysMap {
		metricKeys = append(metricKeys, key)
	}

	return metricKeys
}

func getLatestMetrics(transaction *gorm.DB, runID string, metricKeys []string) ([]models.LatestMetric, error) {
	const batchSize = 500

	latestMetrics := make([]models.LatestMetric, 0, len(metricKeys))

	for skip := 0; skip < len(metricKeys); skip += batchSize {
		take := int(math.Max(float64(skip+batchSize), float64(len(metricKeys))))
		if take > len(metricKeys) {
			take = len(metricKeys)
		}

		currentBatch := make([]models.LatestMetric, 0, take-skip)
		keys := metricKeys[skip:take]

		err := transaction.
			Model(&models.LatestMetric{}).
			Where("run_uuid = ?", runID).Where("key IN ?", keys).
			Clauses(clause.Locking{Strength: "UPDATE"}). // https://gorm.io/docs/advanced_query.html#Locking
			Order("run_uuid").
			Order("key").
			Find(&currentBatch).Error
		if err != nil {
			return latestMetrics, fmt.Errorf(
				"failed to get latest metrics for run_uuid %q, skip %d, take %d : %w",
				runID, skip, take, err,
			)
		}

		latestMetrics = append(latestMetrics, currentBatch...)
	}

	return latestMetrics, nil
}

func isNewerMetric(a models.Metric, b models.LatestMetric) bool {
	return a.Step > b.Step ||
		(a.Step == b.Step && a.Timestamp > b.Timestamp) ||
		(a.Step == b.Step && a.Timestamp == b.Timestamp && a.Value > b.Value)
}

//nolint:cyclop
func updateLatestMetricsIfNecessary(transaction *gorm.DB, runID string, metrics []models.Metric) error {
	if len(metrics) == 0 {
		return nil
	}

	metricKeys := getDistinctMetricKeys(metrics)

	latestMetrics, err := getLatestMetrics(transaction, runID, metricKeys)
	if err != nil {
		return fmt.Errorf("failed to get latest metrics for run_uuid %q: %w", runID, err)
	}

	latestMetricsMap := make(map[string]models.LatestMetric, len(latestMetrics))
	for _, m := range latestMetrics {
		latestMetricsMap[m.Key] = m
	}

	nextLatestMetricsMap := make(map[string]models.LatestMetric, len(metrics))

	for _, metric := range metrics {
		latestMetric, found := latestMetricsMap[metric.Key]
		nextLatestMetric, alreadyPresent := nextLatestMetricsMap[metric.Key]

		switch {
		case !found && !alreadyPresent:
			// brand new latest metric
			nextLatestMetricsMap[metric.Key] = metric.NewLatestMetricFromProto()
		case !found && alreadyPresent && isNewerMetric(metric, nextLatestMetric):
			// there is no row in the database but the metric is present twice
			// and we need to take the latest one from the batch.
			nextLatestMetricsMap[metric.Key] = metric.NewLatestMetricFromProto()
		case found && isNewerMetric(metric, latestMetric):
			// compare with the row in the database
			nextLatestMetricsMap[metric.Key] = metric.NewLatestMetricFromProto()
		}
	}

	nextLatestMetrics := make([]models.LatestMetric, 0, len(nextLatestMetricsMap))
	for _, nextLatestMetric := range nextLatestMetricsMap {
		nextLatestMetrics = append(nextLatestMetrics, nextLatestMetric)
	}

	if len(nextLatestMetrics) != 0 {
		if err := transaction.Clauses(clause.OnConflict{
			UpdateAll: true,
		}).Create(nextLatestMetrics).Error; err != nil {
			return fmt.Errorf("failed to upsert latest metrics for run_uuid %q: %w", runID, err)
		}
	}

	return nil
}

func (s TrackingSQLStore) logMetricsWithTransaction(
	transaction *gorm.DB, runID string, metrics []*entities.Metric,
) *contract.Error {
	// Duplicate metric values are eliminated
	seenMetrics := make(map[models.Metric]struct{})
	modelMetrics := make([]models.Metric, 0, len(metrics))

	for _, metric := range metrics {
		currentMetric := models.NewMetricFromEntity(runID, metric)
		if _, ok := seenMetrics[*currentMetric]; !ok {
			seenMetrics[*currentMetric] = struct{}{}

			modelMetrics = append(modelMetrics, *currentMetric)
		}
	}

	if err := transaction.Clauses(clause.OnConflict{DoNothing: true}).
		CreateInBatches(modelMetrics, metricsBatchSize).Error; err != nil {
		return contract.NewErrorWith(
			protos.ErrorCode_INTERNAL_ERROR,
			fmt.Sprintf("error creating metrics in batch for run_uuid %q", runID),
			err,
		)
	}

	if err := updateLatestMetricsIfNecessary(transaction, runID, modelMetrics); err != nil {
		return contract.NewErrorWith(
			protos.ErrorCode_INTERNAL_ERROR,
			fmt.Sprintf("error updating latest metrics for run_uuid %q", runID),
			err,
		)
	}

	return nil
}

func (s TrackingSQLStore) logModelMetricsWithTransaction(
	transaction *gorm.DB, runID string, metrics []*entities.Metric,
) *contract.Error {
	// Duplicate metric values are eliminated
	loggedModelMetrics := make([]models.LoggedModelMetric, 0, len(metrics))
	seenLoggedModelMetrics := make(map[models.LoggedModelMetric]struct{})

	for _, metric := range metrics {
		currentMetric := models.NewLoggedMetricFromEntity(runID, metric)
		if _, ok := seenLoggedModelMetrics[*currentMetric]; !ok {
			seenLoggedModelMetrics[*currentMetric] = struct{}{}

			loggedModelMetrics = append(loggedModelMetrics, *currentMetric)
		}
	}

	if err := transaction.Clauses(
		clause.OnConflict{DoNothing: true},
	).CreateInBatches(
		loggedModelMetrics, metricsBatchSize,
	).Error; err != nil {
		return contract.NewErrorWith(
			protos.ErrorCode_INTERNAL_ERROR,
			fmt.Sprintf("error creating logged model metrics in batch for run_uuid %q", runID),
			err,
		)
	}

	return nil
}

func (s TrackingSQLStore) LogMetric(ctx context.Context, runID string, metric *entities.Metric) *contract.Error {
	err := s.db.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		contractError := checkRunIsActive(transaction, runID)
		if contractError != nil {
			return contractError
		}

		if err := s.logMetricsWithTransaction(transaction, runID, []*entities.Metric{
			metric,
		}); err != nil {
			return err
		}

		if err := s.logModelMetricsWithTransaction(transaction, runID, []*entities.Metric{
			metric,
		}); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		var contractError *contract.Error
		if errors.As(err, &contractError) {
			return contractError
		}

		return contract.NewErrorWith(
			protos.ErrorCode_INTERNAL_ERROR,
			fmt.Sprintf("log metric transaction failed for %q", runID),
			err,
		)
	}

	return nil
}

func (s TrackingSQLStore) GetMetricHistory(
	ctx context.Context, runID, metricKey string,
) ([]*entities.Metric, *contract.Error) {
	var metrics []*models.Metric
	if err := s.db.WithContext(
		ctx,
	).Where(
		"run_uuid = ?", runID,
	).Where(
		"key = ?", metricKey,
	).Find(&metrics).Error; err != nil {
		return nil, contract.NewError(
			protos.ErrorCode_INTERNAL_ERROR, fmt.Sprintf("error getting metric history: %v", err),
		)
	}

	entityMetrics := make([]*entities.Metric, len(metrics))
	for i, metric := range metrics {
		entityMetrics[i] = metric.ToEntity()
	}

	return entityMetrics, nil
}
