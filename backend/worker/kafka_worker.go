package worker

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/highlight-run/highlight/backend/clickhouse"
	"github.com/highlight-run/highlight/backend/email"
	"github.com/highlight-run/highlight/backend/env"
	kafka_queue "github.com/highlight-run/highlight/backend/kafka-queue"
	kafkaqueue "github.com/highlight-run/highlight/backend/kafka-queue"
	"github.com/highlight-run/highlight/backend/model"
	privateModel "github.com/highlight-run/highlight/backend/private-graph/graph/model"
	pubgraph "github.com/highlight-run/highlight/backend/public-graph/graph"
	"github.com/highlight-run/highlight/backend/util"
	hmetric "github.com/highlight/highlight/sdk/highlight-go/metric"
	e "github.com/pkg/errors"
	"github.com/samber/lo"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/trace"
)

func (k *KafkaWorker) processWorkerError(ctx context.Context, task kafkaqueue.RetryableMessage, err error, start time.Time) {
	log.WithContext(ctx).
		WithError(err).
		WithField("type", task.GetType()).
		WithField("duration", time.Since(start).Seconds()).
		Errorf("task %+v failed: %s", task, err)
	if task.GetFailures() >= task.GetMaxRetries() {
		log.WithContext(ctx).
			WithError(err).
			WithField("type", task.GetType()).
			WithField("failures", task.GetFailures()).
			WithField("duration", time.Since(start).Seconds()).
			Errorf("task %+v failed after %d retries", task, task.GetFailures())
	} else {
		hmetric.Histogram(ctx, "worker.kafka.processed.taskFailures", float64(task.GetFailures()), nil, 1)
	}
	task.SetFailures(task.GetFailures() + 1)
}

func (k *KafkaWorker) log(ctx context.Context, task kafkaqueue.RetryableMessage, msg ...interface{}) {
	if task == nil {
		return
	}
	m := task.GetKafkaMessage()
	if m == nil {
		return
	}
	if m.Partition%100 == 0 {
		log.WithContext(ctx).
			WithField("key", string(m.Key)).
			WithField("offset", m.Offset).
			WithField("taskType", task.GetType()).
			WithField("partition", m.Partition).
			Debug(msg...)
	}
}

func (k *KafkaWorker) ProcessMessages() {
	for {
		func(ctx context.Context) {
			var err error
			defer util.Recover()

			spanKind := trace.SpanKindConsumer
			// sample-in 1/1000 of ProcessPayload calls
			if env.IsProduction() && pubgraph.IsIngestedBySample(ctx, "", 1./1000) {
				spanKind = trace.SpanKindServer
			}

			s, ctx := util.StartSpanFromContext(ctx, "worker.kafka.process", util.WithSpanKind(spanKind))
			s.SetAttribute("worker.goroutine", k.WorkerThread)
			defer s.Finish(err)

			s1, _ := util.StartSpanFromContext(ctx, "worker.kafka.receiveMessage")
			ctx, task := k.KafkaQueue.Receive(ctx)
			s1.Finish()

			if task == nil {
				return
			} else if task.GetType() == kafkaqueue.HealthCheck {
				return
			}
			s.SetAttribute("taskType", task.GetType())
			s.SetAttribute("partition", task.GetKafkaMessage().Partition)
			s.SetAttribute("offset", task.GetKafkaMessage().Offset)
			s.SetAttribute("partitionKey", string(task.GetKafkaMessage().Key))
			k.log(ctx, task, "received message")

			// When the propagated span is extracted from the Kafka message, it is a non-recording span.
			// Set the SpanKind here again.
			s2, sCtx := util.StartSpanFromContext(ctx, "worker.kafka.processMessage", util.WithSpanKind(spanKind))
			for i := 0; i <= task.GetMaxRetries(); i++ {
				k.log(ctx, task, "starting processing ", i)
				start := time.Now()
				publicWorkerMessage, ok := task.(*kafka_queue.Message)
				if !ok {
					k.processWorkerError(ctx, task, e.New("failed to cast as publicWorkerMessage"), start)
					break
				}
				if err = k.Worker.processPublicWorkerMessage(sCtx, publicWorkerMessage); err != nil {
					k.processWorkerError(ctx, task, err, start)
				} else {
					break
				}
			}
			k.log(ctx, task, "finished processing ", task.GetFailures())
			s.SetAttribute("taskFailures", task.GetFailures())
			s2.Finish(err)

			s3, _ := util.StartSpanFromContext(ctx, "worker.kafka.commitMessage")
			k.KafkaQueue.Commit(ctx, task.GetKafkaMessage())
			k.log(ctx, task, "committed")
			s3.Finish()

			hmetric.Incr(ctx, "worker.kafka.processed.count", nil, 1)
		}(context.Background())
	}
}

// DefaultBatchFlushSize set per https://clickhouse.com/docs/en/cloud/bestpractices/bulk-inserts
const DefaultBatchFlushSize = 10000
const DefaultBatchedFlushTimeout = 5 * time.Second
const SessionsMaxRowsPostgres = 500
const ErrorGroupsMaxRowsPostgres = 500
const ErrorObjectsMaxRowsPostgres = 500
const MinRetryDelay = 250 * time.Millisecond

type KafkaWorker struct {
	KafkaQueue   *kafkaqueue.Queue
	Worker       *Worker
	WorkerThread int
}

func (k *KafkaBatchWorker) log(ctx context.Context, fields log.Fields, msg ...interface{}) {
	if k.lastPartitionId == nil {
		return
	}

	partitionId := *k.lastPartitionId
	if partitionId%10 == 0 {
		log.WithContext(ctx).
			WithField("worker_name", k.Name).
			WithField("partition", partitionId).
			WithFields(fields).
			Debug(msg...)
	}
}

func (k *KafkaBatchWorker) flush(ctx context.Context) error {
	k.log(ctx, log.Fields{"message_length": len(k.messages)}, "KafkaBatchWorker flushing messages")

	s, ctx := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush", k.Name))
	s.SetAttribute("BatchSize", len(k.messages))
	defer s.Finish()

	var syncSessionIds []int
	var syncErrorGroupIds []int
	var syncErrorObjectIds []int
	var logRows []*clickhouse.LogRow
	var traceRows []*clickhouse.ClickhouseTraceRow
	var sessionEventRows []*clickhouse.SessionEventRow
	var metricRows []clickhouse.MetricRow

	var lastMsg kafkaqueue.RetryableMessage
	var oldestMsg = time.Now()
	readSpan, sCtx := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush.readMessages", k.Name))
	for _, lastMsg = range k.messages {
		if lastMsg.GetKafkaMessage().Time.Before(oldestMsg) {
			oldestMsg = lastMsg.GetKafkaMessage().Time
		}

		publicWorkerMessage, ok := lastMsg.(*kafka_queue.Message)
		if !ok && lastMsg.GetType() != kafkaqueue.PushLogsFlattened && lastMsg.GetType() != kafkaqueue.PushTracesFlattened && lastMsg.GetType() != kafkaqueue.PushOTeLMetricSum && lastMsg.GetType() != kafkaqueue.PushOTeLMetricHistogram && lastMsg.GetType() != kafkaqueue.PushOTeLMetricSummary && lastMsg.GetType() != kafkaqueue.PushSessionEvents {
			log.WithContext(sCtx).Errorf("type assertion failed for *kafka_queue.Message")
			continue
		}

		switch lastMsg.GetType() {
		case kafkaqueue.SessionDataSync:
			syncSessionIds = append(syncSessionIds, publicWorkerMessage.SessionDataSync.SessionID)
		case kafkaqueue.ErrorGroupDataSync:
			syncErrorGroupIds = append(syncErrorGroupIds, publicWorkerMessage.ErrorGroupDataSync.ErrorGroupID)
		case kafkaqueue.ErrorObjectDataSync:
			syncErrorObjectIds = append(syncErrorObjectIds, publicWorkerMessage.ErrorObjectDataSync.ErrorObjectID)
		case kafkaqueue.PushLogsFlattened:
			logRow, ok := lastMsg.(*kafka_queue.LogRowMessage)
			if !ok {
				log.WithContext(sCtx).Errorf("type assertion failed for *kafka_queue.LogRowMessage")
				continue
			}
			if logRow != nil {
				logRows = append(logRows, logRow.LogRow)
			}
		case kafkaqueue.PushTracesFlattened:
			traceRow, ok := lastMsg.(*kafka_queue.TraceRowMessage)
			if !ok {
				log.WithContext(sCtx).Errorf("type assertion failed for *kafka_queue.TraceRowMessage")
				continue
			}
			if traceRow != nil {
				traceRows = append(traceRows, traceRow.ClickhouseTraceRow)
			}
		case kafkaqueue.PushLogs:
			logRow := publicWorkerMessage.PushLogs.LogRow
			if logRow != nil {
				logRows = append(logRows, logRow)
			}
		case kafkaqueue.PushTraces:
			traceRow := publicWorkerMessage.PushTraces.TraceRow
			if traceRow != nil {
				traceRows = append(traceRows, clickhouse.ConvertTraceRow(traceRow))
			}
		case kafkaqueue.PushSessionEvents:
			sessionEventRow, ok := lastMsg.(*kafka_queue.SessionEventRowMessage)
			if !ok {
				log.WithContext(sCtx).Errorf("type assertion failed for *kafka_queue.SessionEventRowMessage")
				continue
			}
			if sessionEventRow != nil {
				sessionEventRows = append(sessionEventRows, sessionEventRow.SessionEventRow)
			}
		case kafkaqueue.PushOTeLMetricSum:
			metricRow := lastMsg.(*kafka_queue.OTeLMetricSumRow)
			if metricRow != nil && metricRow.MetricSumRow != nil {
				metricRows = append(metricRows, metricRow.MetricSumRow)
			}
		case kafkaqueue.PushOTeLMetricHistogram:
			metricRow := lastMsg.(*kafka_queue.OTeLMetricHistogramRow)
			if metricRow != nil && metricRow.MetricHistogramRow != nil {
				metricRows = append(metricRows, metricRow.MetricHistogramRow)
			}
		case kafkaqueue.PushOTeLMetricSummary:
			metricRow := lastMsg.(*kafka_queue.OTeLMetricSummaryRow)
			if metricRow != nil && metricRow.MetricSummaryRow != nil {
				metricRows = append(metricRows, metricRow.MetricSummaryRow)
			}
		default:
			log.WithContext(sCtx).Errorf("unknown message type received by batch worker %+v", lastMsg.GetType())
		}
	}

	k.log(
		sCtx,
		log.Fields{
			"session_ids":           syncSessionIds,
			"error_group_ids":       syncErrorGroupIds,
			"error_object_ids":      syncErrorObjectIds,
			"log_rows_length":       len(logRows),
			"trace_rows_length":     len(traceRows),
			"session_events_length": len(sessionEventRows),
			"metric_rows":           len(metricRows),
		},
		"KafkaBatchWorker organized messages",
	)

	k.messages = []kafka_queue.RetryableMessage{}

	readSpan.SetAttribute("MaxIngestDelay", time.Since(oldestMsg).Seconds())
	readSpan.Finish()

	workSpan, sCtx := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush.work", k.Name))
	if len(syncSessionIds) > 0 || len(syncErrorGroupIds) > 0 || len(syncErrorObjectIds) > 0 {
		if err := k.flushDataSync(sCtx, syncSessionIds, syncErrorGroupIds, syncErrorObjectIds); err != nil {
			workSpan.Finish(err)
			return err
		}
	}
	if len(logRows) > 0 {
		if err := k.flushLogs(sCtx, logRows); err != nil {
			workSpan.Finish(err)
			return err
		}
	}
	if len(traceRows) > 0 {
		if err := k.flushTraces(sCtx, traceRows); err != nil {
			workSpan.Finish(err)
			return err
		}
	}
	if len(sessionEventRows) > 0 {
		if err := k.flushSessionEvents(sCtx, sessionEventRows); err != nil {
			workSpan.Finish(err)
			return err
		}
	}
	if len(metricRows) > 0 {
		if err := k.flushMetrics(sCtx, metricRows); err != nil {
			workSpan.Finish(err)
			return err
		}
	}
	workSpan.Finish()

	commitSpan, sCtx := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush.commit", k.Name))
	if lastMsg != nil {
		k.KafkaQueue.Commit(sCtx, lastMsg.GetKafkaMessage())
	}
	commitSpan.Finish()

	return nil
}

func (k *KafkaBatchWorker) getQuotaExceededByProject(ctx context.Context, projectIds map[uint32]struct{}, productType model.PricingProductType) (map[uint32]bool, error) {
	spanW, ctxW := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush.checkBillingQuotas", k.Name))

	// If it's saved in Redis that a project has exceeded / not exceeded
	// its quota, use that value. Else, add the projectId to a list of
	// projects to query.
	quotaExceededByProject := map[uint32]bool{}
	projectsToQuery := []uint32{}
	for projectId := range projectIds {
		exceeded, err := k.Worker.Resolver.Redis.IsBillingQuotaExceeded(ctxW, int(projectId), productType)
		if err != nil {
			log.WithContext(ctxW).Error(err)
			continue
		}
		if exceeded != nil {
			quotaExceededByProject[projectId] = *exceeded
		} else {
			projectsToQuery = append(projectsToQuery, projectId)
		}
	}

	// For any projects to query, get the associated workspace,
	// check if that workspace is within the quota,
	// and write the result to redis.
	for _, projectId := range projectsToQuery {
		project, err := k.Worker.Resolver.Store.GetProject(ctx, int(projectId))
		if err != nil {
			log.WithContext(ctxW).Error(e.Wrap(err, "error querying project"))
			continue
		}

		var workspace model.Workspace
		if err := k.Worker.Resolver.DB.WithContext(ctx).Model(&workspace).
			Where("id = ?", project.WorkspaceID).Find(&workspace).Error; err != nil {
			log.WithContext(ctxW).Error(e.Wrap(err, "error querying workspace"))
			continue
		}

		projects := []model.Project{}
		if err := k.Worker.Resolver.DB.WithContext(ctx).Order("name ASC").Model(&workspace).Association("Projects").Find(&projects); err != nil {
			log.WithContext(ctxW).Error(e.Wrap(err, "error querying associated projects"))
			continue
		}
		workspace.Projects = projects

		withinBillingQuota, quotaPercent := k.Worker.PublicResolver.IsWithinQuota(ctxW, productType, &workspace, time.Now())
		quotaExceededByProject[projectId] = !withinBillingQuota
		if err := k.Worker.Resolver.Redis.SetBillingQuotaExceeded(ctxW, int(projectId), productType, !withinBillingQuota); err != nil {
			log.WithContext(ctxW).Error(err)
			return nil, err
		}

		// Send alert emails if above the relevant thresholds
		go func() {
			defer util.Recover()
			var emailType email.EmailType
			if quotaPercent >= 1 {
				if productType == model.PricingProductTypeLogs {
					emailType = email.BillingLogsUsage100Percent
				} else if productType == model.PricingProductTypeTraces {
					emailType = email.BillingTracesUsage100Percent
				}
			} else if quotaPercent >= .8 {
				if productType == model.PricingProductTypeLogs {
					emailType = email.BillingLogsUsage80Percent
				} else if productType == model.PricingProductTypeTraces {
					emailType = email.BillingTracesUsage80Percent
				}
			}

			if emailType != "" {
				if err := model.SendBillingNotifications(ctx, k.Worker.PublicResolver.DB, k.Worker.PublicResolver.MailClient, emailType, &workspace, nil); err != nil {
					log.WithContext(ctx).Error(e.Wrap(err, "failed to send billing notifications"))
				}
			}
		}()
	}

	spanW.Finish()

	return quotaExceededByProject, nil
}

func (k *KafkaBatchWorker) flushLogs(ctx context.Context, logRows []*clickhouse.LogRow) error {
	projectIds := map[uint32]struct{}{}
	for _, row := range logRows {
		projectIds[row.ProjectId] = struct{}{}
	}

	quotaExceededByProject, err := k.getQuotaExceededByProject(ctx, projectIds, model.PricingProductTypeLogs)
	if err != nil {
		return err
	}

	var markBackendSetupProjectIds []uint32
	var filteredRows []*clickhouse.LogRow
	for _, logRow := range logRows {
		// create service record for any services found in ingested logs
		if logRow.ServiceName != "" {
			spanX, ctxX := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.upsertService", k.Name))

			project, err := k.Worker.Resolver.Store.GetProject(ctx, int(logRow.ProjectId))
			if err == nil && project != nil {
				_, err := k.Worker.Resolver.Store.UpsertService(ctx, project.ID, logRow.ServiceName, logRow.LogAttributes)

				if err != nil {
					log.WithContext(ctxX).Error(e.Wrap(err, "failed to create service"))
				}
			}

			spanX.Finish()
		}

		if logRow.Source == privateModel.LogSourceBackend {
			markBackendSetupProjectIds = append(markBackendSetupProjectIds, logRow.ProjectId)
		}

		// Filter out any log rows for projects where the log quota has been exceeded
		if quotaExceededByProject[logRow.ProjectId] {
			continue
		}

		// Temporarily filter NextJS logs
		// TODO - remove this condition when https://github.com/highlight/highlight/issues/6181 is fixed
		if !strings.HasPrefix(logRow.Body, "ENOENT: no such file or directory") && !strings.HasPrefix(logRow.Body, "connect ECONNREFUSED") {
			filteredRows = append(filteredRows, logRow)
		}
	}

	wSpan, wCtx := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush.process", k.Name))
	wSpan.SetAttribute("BatchSize", len(k.messages))
	wSpan.SetAttribute("NumProjects", len(projectIds))
	for _, projectId := range markBackendSetupProjectIds {
		err := k.Worker.PublicResolver.MarkBackendSetupImpl(wCtx, int(projectId), model.MarkBackendSetupTypeLogs)
		if err != nil {
			log.WithContext(wCtx).WithError(err).Error("failed to mark backend logs setup")
			return err
		}
	}

	span, ctxT := util.StartSpanFromContext(wCtx, fmt.Sprintf("worker.kafka.%s.flush.clickhouse.logs", k.Name))
	span.SetAttribute("NumLogRows", len(logRows))
	span.SetAttribute("NumFilteredRows", len(filteredRows))
	err = k.Worker.PublicResolver.Clickhouse.BatchWriteLogRows(ctxT, filteredRows)
	span.Finish(err)
	if err != nil {
		log.WithContext(ctxT).WithError(err).Error("failed to batch write logs to clickhouse")
		return err
	}
	wSpan.Finish()
	return nil
}

func (k *KafkaBatchWorker) flushTraces(ctx context.Context, traceRows []*clickhouse.ClickhouseTraceRow) error {
	markBackendSetupProjectIds := map[uint32]struct{}{}
	projectIds := map[uint32]struct{}{}
	for _, trace := range traceRows {
		// Skip traces with a `http.method` attribute as likely autoinstrumented frontend traces
		if _, found := trace.TraceAttributes["http.method"]; !found {
			markBackendSetupProjectIds[trace.ProjectId] = struct{}{}
		}
		projectIds[trace.ProjectId] = struct{}{}
	}

	quotaExceededByProject, err := k.getQuotaExceededByProject(ctx, projectIds, model.PricingProductTypeLogs)
	if err != nil {
		return err
	}

	filteredTraceRows := []*clickhouse.ClickhouseTraceRow{}
	for _, trace := range traceRows {
		if quotaExceededByProject[trace.ProjectId] {
			continue
		}
		filteredTraceRows = append(filteredTraceRows, trace)
	}

	span, ctxT := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush.clickhouse.traces", k.Name), util.WithHighlightTracingDisabled(true))
	span.SetAttribute("NumTraceRows", len(traceRows))
	span.SetAttribute("PayloadSizeBytes", binary.Size(traceRows))
	err = k.Worker.PublicResolver.Clickhouse.BatchWriteTraceRows(ctxT, filteredTraceRows)
	defer span.Finish(err)
	if err != nil {
		log.WithContext(ctxT).WithError(err).Error("failed to batch write traces to clickhouse")
		span.Finish(err)
		return err
	}

	for projectId := range markBackendSetupProjectIds {
		err := k.Worker.PublicResolver.MarkBackendSetupImpl(ctx, int(projectId), model.MarkBackendSetupTypeTraces)
		if err != nil {
			log.WithContext(ctx).WithError(err).Error("failed to mark backend traces setup")
			return err
		}
	}
	return nil
}

func (k *KafkaBatchWorker) flushSessionEvents(ctx context.Context, sessionEventRows []*clickhouse.SessionEventRow) error {
	span, ctxT := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush.clickhouse.events", k.Name))
	span.SetAttribute("NumRows", len(sessionEventRows))
	span.SetAttribute("PayloadSizeBytes", binary.Size(sessionEventRows))
	err := k.Worker.PublicResolver.Clickhouse.BatchWriteSessionEventRows(ctxT, sessionEventRows)
	defer span.Finish(err)
	if err != nil {
		log.WithContext(ctxT).WithError(err).Error("failed to batch write session events to clickhouse")
		span.Finish(err)
		return err
	}

	return nil
}

func (k *KafkaBatchWorker) flushMetrics(ctx context.Context, metricRows []clickhouse.MetricRow) error {
	span, ctxT := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush.clickhouse.metrics", k.Name))
	span.SetAttribute("NumRows", len(metricRows))
	span.SetAttribute("PayloadSizeBytes", binary.Size(metricRows))
	err := k.Worker.PublicResolver.Clickhouse.BatchWriteMetricRows(ctxT, metricRows)
	defer span.Finish(err)
	if err != nil {
		log.WithContext(ctxT).WithError(err).Error("failed to batch write metrics to clickhouse")
		span.Finish(err)
		return err
	}

	return nil
}

func (k *KafkaBatchWorker) flushDataSync(ctx context.Context, sessionIds []int, errorGroupIds []int, errorObjectIds []int) error {
	sessionIdChunks := lo.Chunk(lo.Uniq(sessionIds), SessionsMaxRowsPostgres)
	if len(sessionIdChunks) > 0 {
		k.log(ctx, log.Fields{"session_ids": sessionIds}, "KafkaBatchWorker flushing sessions")

		allSessionObjs := []*model.Session{}
		for _, chunk := range sessionIdChunks {
			k.log(ctx, log.Fields{"session_ids": chunk}, "KafkaBatchWorker flushing session chunk")

			sessionObjs := []*model.Session{}
			sessionSpan, _ := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush.readSessions", k.Name))
			if err := k.Worker.PublicResolver.DB.WithContext(ctx).Model(&model.Session{}).Preload("ViewedByAdmins").Where("id in ?", chunk).Find(&sessionObjs).Error; err != nil {
				log.WithContext(ctx).Error(err)
				return err
			}
			sessionSpan.Finish()

			for _, session := range sessionObjs {
				var err error
				session.Fields, err = k.Worker.Resolver.Redis.GetSessionFields(ctx, session.SecureID)
				if err != nil {
					log.WithContext(ctx).Error(err)
					return err
				}
			}

			allSessionObjs = append(allSessionObjs, sessionObjs...)
		}

		chSpan, _ := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.flush.clickhouse.sessions", k.Name))

		k.log(ctx, log.Fields{"sessions_length": len(allSessionObjs)}, "KafkaBatchWorker writing sessions")

		err := k.Worker.PublicResolver.Clickhouse.WriteSessions(ctx, allSessionObjs)
		defer chSpan.Finish(err)
		if err != nil {
			log.WithContext(ctx).Error(err)
			return err
		}
	}

	errorGroupIdChunks := lo.Chunk(lo.Uniq(errorGroupIds), ErrorGroupsMaxRowsPostgres)
	if len(errorGroupIdChunks) > 0 {
		k.log(ctx, log.Fields{"error_group_ids": errorGroupIds}, "KafkaBatchWorker flushing error groups")

		allErrorGroups := []*model.ErrorGroup{}
		for _, chunk := range errorGroupIdChunks {
			k.log(ctx, log.Fields{"error_group_ids": chunk}, "KafkaBatchWorker flushing error groups chunk")

			errorGroups := []*model.ErrorGroup{}
			errorGroupSpan, _ := util.StartSpanFromContext(ctx, "worker.kafka.datasync.readErrorGroups")
			if err := k.Worker.PublicResolver.DB.WithContext(ctx).Model(&model.ErrorGroup{}).Joins("ErrorTag").Where("error_groups.id in ?", chunk).Find(&errorGroups).Error; err != nil {
				log.WithContext(ctx).Error(err)
				return err
			}
			errorGroupSpan.Finish()

			allErrorGroups = append(allErrorGroups, errorGroups...)
		}

		chSpan, _ := util.StartSpanFromContext(ctx, "worker.kafka.datasync.writeClickhouse.errorGroups")
		k.log(ctx, log.Fields{"error_groups_length": len(allErrorGroups)}, "KafkaBatchWorker writing error groups")

		if err := k.Worker.PublicResolver.Clickhouse.WriteErrorGroups(ctx, allErrorGroups); err != nil {
			log.WithContext(ctx).Error(err)
			return err
		}
		chSpan.Finish()
	}

	errorObjectIdChunks := lo.Chunk(lo.Uniq(errorObjectIds), ErrorObjectsMaxRowsPostgres)
	if len(errorObjectIdChunks) > 0 {
		k.log(ctx, log.Fields{"error_objects_ids": errorObjectIds}, "KafkaBatchWorker flushing error objects")

		allErrorObjects := []*model.ErrorObject{}
		for _, chunk := range errorObjectIdChunks {
			k.log(ctx, log.Fields{"error_objects_ids": chunk}, "KafkaBatchWorker flushing error objects chunk")

			errorObjects := []*model.ErrorObject{}
			errorObjectSpan, _ := util.StartSpanFromContext(ctx, "worker.kafka.datasync.readErrorObjects")
			if err := k.Worker.PublicResolver.DB.WithContext(ctx).Model(&model.ErrorObject{}).Where("id in ?", chunk).Find(&errorObjects).Error; err != nil {
				log.WithContext(ctx).Error(err)
				return err
			}
			errorObjectSpan.Finish()

			allErrorObjects = append(allErrorObjects, errorObjects...)
		}

		// error objects -> filter non nil -> get session id -> get unique
		uniqueSessionIds := lo.Uniq(lo.Map(lo.Filter(
			allErrorObjects,
			func(eo *model.ErrorObject, _ int) bool { return eo.SessionID != nil }),
			func(eo *model.ErrorObject, _ int) int { return *eo.SessionID }))
		sessionIdChunks := lo.Chunk(uniqueSessionIds, SessionsMaxRowsPostgres)
		allSessions := []*model.Session{}
		for _, chunk := range sessionIdChunks {
			sessions := []*model.Session{}
			sessionSpan, _ := util.StartSpanFromContext(ctx, "worker.kafka.datasync.readErrorObjectSessions")
			if err := k.Worker.PublicResolver.DB.WithContext(ctx).Model(&model.Session{}).Where("id in ?", chunk).Find(&sessions).Error; err != nil {
				log.WithContext(ctx).Error(err)
				return err
			}
			sessionSpan.Finish()

			allSessions = append(allSessions, sessions...)
		}

		chSpan, _ := util.StartSpanFromContext(ctx, "worker.kafka.datasync.writeClickhouse.errorObjects")

		k.log(ctx, log.Fields{"error_object_length": len(allErrorObjects), "sessions_length": len(allSessions)}, "KafkaBatchWorker writing error objects")

		if err := k.Worker.PublicResolver.Clickhouse.WriteErrorObjects(ctx, allErrorObjects, allSessions); err != nil {
			log.WithContext(ctx).Error(err)
			return err
		}
		chSpan.Finish()
	}

	return nil
}

func (k *KafkaBatchWorker) processWorkerError(ctx context.Context, attempt int, err error) {
	log.WithContext(ctx).WithError(err).WithField("worker_name", k.Name).WithField("attempt", attempt).Errorf("batched worker task failed: %s", err)
	// exponential backoff on retries
	time.Sleep(MinRetryDelay * time.Duration(math.Pow(2, float64(attempt))))
}

func (k *KafkaBatchWorker) ProcessMessages() {
	for {
		func(ctx context.Context) {
			defer util.Recover()
			s, ctx := util.StartSpanFromContext(ctx, util.KafkaBatchWorkerOp, util.ResourceName(fmt.Sprintf("worker.kafka.%s.process", k.Name)), util.WithHighlightTracingDisabled(k.TracingDisabled), util.WithSpanKind(trace.SpanKindConsumer))
			s.SetAttribute("worker.goroutine", k.WorkerThread)
			s.SetAttribute("BatchSize", len(k.messages))
			defer s.Finish()

			s1, _ := util.StartSpanFromContext(ctx, fmt.Sprintf("worker.kafka.%s.receive", k.Name))
			// wait for up to k.BatchedFlushTimeout to receive a message
			// before proceeding to flush previously batched messages
			// and restarting the receive call
			receiveCtx, receiveCancel := context.WithTimeout(ctx, k.BatchedFlushTimeout)
			defer receiveCancel()
			ctx, task := k.KafkaQueue.Receive(receiveCtx)
			s1.Finish()

			if task != nil {
				k.lastPartitionId = &task.GetKafkaMessage().Partition
				if task.GetType() != kafkaqueue.HealthCheck {
					k.messages = append(k.messages, task)
				}
			}

			if time.Since(k.lastFlush) > k.BatchedFlushTimeout || len(k.messages) >= k.BatchFlushSize {
				s.SetAttribute("FlushDelay", time.Since(k.lastFlush).Seconds())

				for i := 0; i <= kafkaqueue.TaskRetries; i++ {
					if err := k.flush(ctx); err != nil {
						k.processWorkerError(ctx, i, err)
					} else {
						break
					}
				}
				k.lastFlush = time.Now()
			}
		}(context.Background())
	}
}

type KafkaBatchWorker struct {
	KafkaQueue          *kafkaqueue.Queue
	Worker              *Worker
	WorkerThread        int
	BatchFlushSize      int
	BatchedFlushTimeout time.Duration
	Name                string
	TracingDisabled     bool

	lastFlush       time.Time
	messages        []kafkaqueue.RetryableMessage
	lastPartitionId *int
}
