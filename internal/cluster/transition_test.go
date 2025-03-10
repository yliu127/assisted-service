package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/openshift/assisted-service/internal/common"
	"github.com/openshift/assisted-service/internal/constants"
	"github.com/openshift/assisted-service/internal/dns"
	"github.com/openshift/assisted-service/internal/events"
	"github.com/openshift/assisted-service/internal/host"
	"github.com/openshift/assisted-service/internal/metrics"
	"github.com/openshift/assisted-service/internal/operators"
	"github.com/openshift/assisted-service/models"
	"github.com/openshift/assisted-service/pkg/ocm"
	"github.com/openshift/assisted-service/pkg/s3wrapper"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/thoas/go-funk"
)

var _ = Describe("Transition tests", func() {
	var (
		ctx              = context.Background()
		capi             API
		db               *gorm.DB
		clusterId        strfmt.UUID
		eventsHandler    events.Handler
		ctrl             *gomock.Controller
		mockMetric       *metrics.MockAPI
		dbName           string
		operatorsManager *operators.MockAPI
		mockS3Api        *s3wrapper.MockAPI
		mockAccountsMgmt *ocm.MockOCMAccountsMgmt
	)

	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		eventsHandler = events.New(db, logrus.New())
		ctrl = gomock.NewController(GinkgoT())
		mockMetric = metrics.NewMockAPI(ctrl)
		mockS3Api = s3wrapper.NewMockAPI(ctrl)
		mockAccountsMgmt = ocm.NewMockOCMAccountsMgmt(ctrl)
		clusterId = strfmt.UUID(uuid.New().String())
	})

	Context("cancel_installation", func() {
		BeforeEach(func() {
			capi = NewManager(getDefaultConfig(), common.GetTestLog(), db, eventsHandler, nil, mockMetric, nil, nil, operatorsManager, nil, nil, nil)
		})

		It("cancel_installation", func() {
			c := common.Cluster{
				Cluster: models.Cluster{ID: &clusterId, Status: swag.String(models.ClusterStatusInstalling)},
			}
			Expect(db.Create(&c).Error).ShouldNot(HaveOccurred())
			mockMetric.EXPECT().ClusterInstallationFinished(gomock.Any(), models.ClusterStatusCancelled, models.ClusterStatusInstalling, c.OpenshiftVersion, *c.ID, c.EmailDomain, c.InstallStartedAt)
			Expect(capi.CancelInstallation(ctx, &c, "", db)).ShouldNot(HaveOccurred())

			Expect(db.First(&c, "id = ?", c.ID).Error).ShouldNot(HaveOccurred())
			Expect(swag.StringValue(c.Status)).Should(Equal(models.ClusterStatusCancelled))
		})

		It("cancel_installation_conflict", func() {
			c := common.Cluster{
				Cluster: models.Cluster{ID: &clusterId, Status: swag.String(models.ClusterStatusInsufficient)},
			}
			Expect(db.Create(&c).Error).ShouldNot(HaveOccurred())
			mockMetric.EXPECT().ClusterInstallationFinished(gomock.Any(), models.ClusterStatusCancelled, models.ClusterStatusInsufficient, c.OpenshiftVersion, *c.ID, c.EmailDomain, c.InstallStartedAt)
			replay := capi.CancelInstallation(ctx, &c, "", db)
			Expect(replay).Should(HaveOccurred())
			Expect(int(replay.StatusCode())).Should(Equal(http.StatusConflict))

			Expect(db.First(&c, "id = ?", c.ID).Error).ShouldNot(HaveOccurred())
			Expect(swag.StringValue(c.Status)).Should(Equal(models.ClusterStatusInsufficient))
		})

		It("cancel_failed_installation", func() {
			c := common.Cluster{
				Cluster: models.Cluster{
					ID:         &clusterId,
					StatusInfo: swag.String("original error"),
					Status:     swag.String(models.ClusterStatusError)},
			}
			Expect(db.Create(&c).Error).ShouldNot(HaveOccurred())
			mockMetric.EXPECT().ClusterInstallationFinished(gomock.Any(), models.ClusterStatusCancelled, models.ClusterStatusError, c.OpenshiftVersion, *c.ID, c.EmailDomain, c.InstallStartedAt)
			Expect(capi.CancelInstallation(ctx, &c, "", db)).ShouldNot(HaveOccurred())

			Expect(db.First(&c, "id = ?", c.ID).Error).ShouldNot(HaveOccurred())
			Expect(swag.StringValue(c.Status)).Should(Equal(models.ClusterStatusCancelled))
			Expect(swag.StringValue(c.StatusInfo)).ShouldNot(Equal("original error"))
		})
	})
	Context("complete_installation", func() {
		tests := []struct {
			name                         string
			operators                    []*models.MonitoredOperator
			uploadKubeConfig             bool
			updateAMSSubscription        bool
			updateAMSSubscriptionSuccess bool
			errorExpected                bool
			updateSuccessfullyFinished   bool
			destState                    string
			destStatusInfo               string
		}{
			{
				name:      "no change -> finalizing",
				destState: models.ClusterStatusFinalizing,
			},
			// TODO: MGMT-4458
			// Backward-compatible solution for clusters that don't have monitored operators data
			// Those clusters shouldn't finish until the controller would tell them.
			{
				name:                       "no operators (backward-compatability) -> installed",
				uploadKubeConfig:           true,
				updateSuccessfullyFinished: true,
				destState:                  models.ClusterStatusInstalled,
				destStatusInfo:             statusInfoInstalled,
			},
			{
				name:                       "available builtin operators, no OLM -> installed",
				uploadKubeConfig:           true,
				updateSuccessfullyFinished: true,
				operators: []*models.MonitoredOperator{
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name, OperatorType: models.OperatorTypeBuiltin,
						Status: models.OperatorStatusAvailable,
					},
				},
				destState:      models.ClusterStatusInstalled,
				destStatusInfo: statusInfoInstalled,
			},
			{
				name:                       "progressing builtin operator, no OLM -> finalizing",
				uploadKubeConfig:           true,
				updateSuccessfullyFinished: false,
				operators: []*models.MonitoredOperator{
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name, OperatorType: models.OperatorTypeBuiltin,
						Status: models.OperatorStatusProgressing,
					},
				},

				destState: models.ClusterStatusFinalizing,
			},
			{
				name:                       "failed builtin operator, no OLM -> finalizing",
				uploadKubeConfig:           true,
				updateSuccessfullyFinished: false,
				operators: []*models.MonitoredOperator{
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name, OperatorType: models.OperatorTypeBuiltin,
						Status: models.OperatorStatusFailed,
					},
				},

				destState: models.ClusterStatusFinalizing,
			},
			{
				name:                       "available builtin operators, progress OLM -> finalizing",
				uploadKubeConfig:           true,
				updateSuccessfullyFinished: true,
				operators: []*models.MonitoredOperator{
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name, OperatorType: models.OperatorTypeBuiltin,
						Status: models.OperatorStatusAvailable,
					},
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name + "2", OperatorType: models.OperatorTypeOlm,
						Status: models.OperatorStatusProgressing,
					},
				},
				destState: models.ClusterStatusFinalizing,
			},
			{
				name:                       "available builtin operators, failed OLM -> installed (degraded)",
				uploadKubeConfig:           true,
				updateSuccessfullyFinished: true,
				operators: []*models.MonitoredOperator{
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name, OperatorType: models.OperatorTypeBuiltin,
						Status: models.OperatorStatusAvailable,
					},
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name + "2", OperatorType: models.OperatorTypeOlm,
						Status: models.OperatorStatusFailed,
					},
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name + "3", OperatorType: models.OperatorTypeOlm,
						Status: models.OperatorStatusAvailable,
					},
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name + "4", OperatorType: models.OperatorTypeOlm,
						Status: models.OperatorStatusFailed,
					},
				},
				destState:      models.ClusterStatusInstalled,
				destStatusInfo: StatusInfoDegraded + ". Failed OLM operators: dummy2, dummy4",
			},
			{
				name:                       "available builtin operators, available OLM -> installed",
				uploadKubeConfig:           true,
				updateSuccessfullyFinished: true,
				operators: []*models.MonitoredOperator{
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name, OperatorType: models.OperatorTypeBuiltin,
						Status: models.OperatorStatusAvailable,
					},
					{
						Name: common.TestDefaultConfig.MonitoredOperator.Name + "2", OperatorType: models.OperatorTypeOlm,
						Status: models.OperatorStatusAvailable,
					},
				},
				destState:      models.ClusterStatusInstalled,
				destStatusInfo: statusInfoInstalled,
			},
			{
				name:                         "success - with AMS -> installed",
				uploadKubeConfig:             true,
				updateSuccessfullyFinished:   true,
				updateAMSSubscription:        true,
				updateAMSSubscriptionSuccess: true,
				destState:                    models.ClusterStatusInstalled,
				destStatusInfo:               statusInfoInstalled,
			},
			{
				name:                         "success - with AMS (update failed) -> finalizing",
				uploadKubeConfig:             true,
				updateSuccessfullyFinished:   true,
				updateAMSSubscription:        true,
				updateAMSSubscriptionSuccess: false,
				errorExpected:                true,
				destState:                    models.ClusterStatusFinalizing,
			},
		}

		checkCompleteInstallationUpdate := func(eventSeverity string, eventMessage string) {
			events, err := eventsHandler.GetEvents(clusterId, nil)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(len(events)).ShouldNot(Equal(0))
			resetEvent := events[len(events)-1]
			Expect(*resetEvent.Severity).Should(Equal(eventSeverity))

			if eventMessage != "" {
				Expect(funk.Contains(*resetEvent.Message, eventMessage)).Should(Equal(true))
			}

			var clusterInfo common.Cluster
			db.First(&clusterInfo)
			completionTime := time.Time(clusterInfo.InstallCompletedAt).In(time.UTC)
			Expect(time.Until(completionTime)).Should(BeNumerically("<", 1*time.Second))
		}

		for i := range tests {
			t := tests[i]
			It(t.name, func() {
				// Setup
				c := common.Cluster{
					Cluster: models.Cluster{
						ID:                 &clusterId,
						Status:             swag.String(models.ClusterStatusFinalizing),
						MonitoredOperators: t.operators,
					},
					IsAmsSubscriptionConsoleUrlSet: true,
				}
				Expect(common.LoadTableFromDB(db, common.MonitoredOperatorsTable).Create(&c).Error).ShouldNot(HaveOccurred())

				var ocmClient *ocm.Client = nil

				if t.updateAMSSubscription {
					ocmClient = &ocm.Client{AccountsMgmt: mockAccountsMgmt, Config: &ocm.Config{}}
				}

				if t.updateSuccessfullyFinished {
					if t.updateAMSSubscription && t.updateAMSSubscriptionSuccess {
						mockAccountsMgmt.EXPECT().UpdateSubscriptionStatusActive(gomock.Any(), gomock.Any()).Return(nil).Times(1)
					} else {
						mockAccountsMgmt.EXPECT().UpdateSubscriptionStatusActive(gomock.Any(), gomock.Any()).Return(errors.New("error")).Times(1)
					}
				}

				mockS3Api.EXPECT().DoesObjectExist(gomock.Any(), gomock.Any()).Return(t.uploadKubeConfig, nil).AnyTimes() // Might be affected by the amount of states

				//duration measurements are always called (even in degraded or failed states)
				mockMetric.EXPECT().ClusterInstallationFinished(gomock.Any(), models.ClusterStatusInstalled, models.ClusterStatusFinalizing, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any())

				capi = NewManager(getDefaultConfig(), common.GetTestLog(), db, eventsHandler, nil, mockMetric, nil, nil, operatorsManager, ocmClient, mockS3Api, nil)

				// Test
				clusterAfterRefresh, err := capi.RefreshStatus(ctx, &c, db)
				if t.errorExpected {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).ToNot(HaveOccurred())
					Expect(swag.StringValue(clusterAfterRefresh.Status)).To(Equal(t.destState))
					Expect(swag.StringValue(clusterAfterRefresh.StatusInfo)).To(Equal(t.destStatusInfo))

					if t.destState != *c.Status {
						if t.updateSuccessfullyFinished {
							checkCompleteInstallationUpdate(models.EventSeverityInfo, "Successfully finished installing cluster")
						} else {
							checkCompleteInstallationUpdate(models.EventSeverityCritical, fmt.Sprintf("Operator %s failed", t.operators[0].Name))
						}
					}
				}

				// Check also the DB
				clusterFromDB := getClusterFromDB(clusterId, db)
				Expect(swag.StringValue(clusterFromDB.Status)).To(Equal(t.destState))
				Expect(swag.StringValue(clusterFromDB.StatusInfo)).To(Equal(t.destStatusInfo))
			})
		}
	})
	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
	})
})

var _ = Describe("Cancel cluster installation", func() {
	var (
		ctx               = context.Background()
		dbName            string
		capi              API
		db                *gorm.DB
		ctrl              *gomock.Controller
		mockEventsHandler *events.MockHandler
		mockMetric        *metrics.MockAPI
	)

	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEventsHandler = events.NewMockHandler(ctrl)
		mockMetric = metrics.NewMockAPI(ctrl)
		operatorsManager := operators.NewManager(common.GetTestLog(), nil, operators.Options{}, nil, nil)
		capi = NewManager(getDefaultConfig(), common.GetTestLog(), db, mockEventsHandler, nil, mockMetric, nil, nil, operatorsManager, nil, nil, nil)
	})

	acceptNewEvents := func(times int) {
		mockEventsHandler.EXPECT().AddEvent(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(times)
	}

	acceptClusterInstallationFinished := func() {
		mockMetric.EXPECT().ClusterInstallationFinished(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(1)
	}

	installationStates := []string{
		models.ClusterStatusPreparingForInstallation, models.ClusterStatusInstalling, models.ClusterStatusFinalizing}

	tests := []struct {
		state      string
		success    bool
		statusCode int32
		eventsNum  int
	}{
		{state: models.ClusterStatusPreparingForInstallation, success: true, eventsNum: 2},
		{state: models.ClusterStatusInstalling, success: true, eventsNum: 2},
		{state: models.ClusterStatusError, success: true, eventsNum: 2},
		{state: models.ClusterStatusFinalizing, success: true, eventsNum: 2},
		{state: models.ClusterStatusInstallingPendingUserAction, success: true, eventsNum: 2},
		{state: models.ClusterStatusInsufficient, success: false, statusCode: http.StatusConflict, eventsNum: 1},
		{state: models.ClusterStatusReady, success: false, statusCode: http.StatusConflict, eventsNum: 1},
		{state: models.ClusterStatusInstalled, success: false, statusCode: http.StatusConflict, eventsNum: 1},
	}

	for _, t := range tests {
		t := t
		It(fmt.Sprintf("cancel from state %s", t.state), func() {
			clusterId := strfmt.UUID(uuid.New().String())
			cluster := common.Cluster{
				Cluster: models.Cluster{ID: &clusterId, Status: swag.String(t.state)},
			}
			Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())

			acceptNewEvents(t.eventsNum)
			if funk.Contains(installationStates, t.state) {
				acceptClusterInstallationFinished()
			}
			err := capi.CancelInstallation(ctx, &cluster, "reason", db)
			if t.success {
				Expect(err).ShouldNot(HaveOccurred())
			} else {
				Expect(err).Should(HaveOccurred())
				Expect(err.StatusCode()).Should(Equal(t.statusCode))
			}
		})
	}

	AfterEach(func() {
		ctrl.Finish()
		common.DeleteTestDB(db, dbName)
	})
})

var _ = Describe("Reset cluster", func() {
	var (
		ctx               = context.Background()
		dbName            string
		capi              API
		db                *gorm.DB
		ctrl              *gomock.Controller
		mockEventsHandler *events.MockHandler
	)

	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEventsHandler = events.NewMockHandler(ctrl)
		operatorsManager := operators.NewManager(common.GetTestLog(), nil, operators.Options{}, nil, nil)
		capi = NewManager(getDefaultConfig(), common.GetTestLog(), db, mockEventsHandler, nil, nil, nil, nil, operatorsManager, nil, nil, nil)
	})

	acceptNewEvents := func(times int) {
		mockEventsHandler.EXPECT().AddEvent(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(times)
	}

	tests := []struct {
		state      string
		success    bool
		statusCode int32
		eventsNum  int
	}{
		{state: models.ClusterStatusPreparingForInstallation, success: true, eventsNum: 2},
		{state: models.ClusterStatusInstalling, success: true, eventsNum: 2},
		{state: models.ClusterStatusError, success: true, eventsNum: 2},
		{state: models.ClusterStatusFinalizing, success: true, eventsNum: 2},
		{state: models.ClusterStatusInstallingPendingUserAction, success: true, eventsNum: 2},
		{state: models.ClusterStatusInsufficient, success: false, statusCode: http.StatusConflict, eventsNum: 1},
		{state: models.ClusterStatusReady, success: false, statusCode: http.StatusConflict, eventsNum: 1},
		{state: models.ClusterStatusInstalled, success: false, statusCode: http.StatusConflict, eventsNum: 1},
	}

	for _, t := range tests {
		t := t
		clusterId := strfmt.UUID(uuid.New().String())
		cluster := common.Cluster{
			Cluster: models.Cluster{ID: &clusterId, Status: swag.String(t.state)},
		}
		It(fmt.Sprintf("resets cluster from state %s", t.state), func() {
			Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
			acceptNewEvents(t.eventsNum)
			err := capi.ResetCluster(ctx, &cluster, "reason", db)
			if t.success {
				Expect(err).ShouldNot(HaveOccurred())
			} else {
				Expect(err).Should(HaveOccurred())
				Expect(err.StatusCode()).Should(Equal(t.statusCode))
			}
		})
		It(fmt.Sprintf("resets API VIP and Ingress VIP in case of single node cluster from state %s", t.state), func() {
			haMode := models.ClusterHighAvailabilityModeNone
			hostIP := "1.2.3.4"
			cluster = common.Cluster{
				Cluster: models.Cluster{
					ID:                   &clusterId,
					Status:               swag.String(t.state),
					HighAvailabilityMode: &haMode,
					APIVip:               hostIP,
					IngressVip:           hostIP,
				},
			}
			Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
			acceptNewEvents(t.eventsNum)
			err := capi.ResetCluster(ctx, &cluster, "reason", db)
			cluster = getClusterFromDB(clusterId, db)
			if t.success {
				Expect(err).ShouldNot(HaveOccurred())
				Expect(cluster.Cluster.APIVip).Should(Equal(""))
				Expect(cluster.Cluster.IngressVip).Should(Equal(""))
			} else {
				Expect(err).Should(HaveOccurred())
				Expect(err.StatusCode()).Should(Equal(t.statusCode))
				Expect(cluster.Cluster.APIVip).ShouldNot(Equal(""))
				Expect(cluster.Cluster.IngressVip).ShouldNot(Equal(""))
			}
		})
	}

	AfterEach(func() {
		ctrl.Finish()
		common.DeleteTestDB(db, dbName)
	})
})

type statusInfoChecker interface {
	check(statusInfo *string)
}

type valueChecker struct {
	value string
}

func (v *valueChecker) check(value *string) {
	if value == nil {
		Expect(v.value).To(Equal(""))
	} else {
		Expect(*value).To(Equal(v.value))
	}
}

func makeValueChecker(value string) statusInfoChecker {
	return &valueChecker{value: value}
}

type validationsChecker struct {
	expected map[ValidationID]validationCheckResult
}

func (j *validationsChecker) check(validationsStr string) {
	validationRes := make(ValidationsStatus)
	Expect(json.Unmarshal([]byte(validationsStr), &validationRes)).ToNot(HaveOccurred())
next:
	for id, checkedResult := range j.expected {
		category, err := id.Category()
		Expect(err).ToNot(HaveOccurred())
		results, ok := validationRes[category]
		Expect(ok).To(BeTrue())
		for _, r := range results {
			if r.ID == id {
				Expect(r.Status).To(Equal(checkedResult.status), "id = %s", id.String())
				Expect(r.Message).To(MatchRegexp(checkedResult.messagePattern))
				continue next
			}
		}
		// Should not reach here
		Expect(false).To(BeTrue())
	}
}

type validationCheckResult struct {
	status         ValidationStatus
	messagePattern string
}

func makeJsonChecker(expected map[ValidationID]validationCheckResult) *validationsChecker {
	return &validationsChecker{expected: expected}
}

var _ = Describe("Refresh Cluster - No DHCP", func() {
	var (
		ctx                                     = context.Background()
		db                                      *gorm.DB
		clusterId, hid1, hid2, hid3, hid4, hid5 strfmt.UUID
		cluster                                 common.Cluster
		clusterApi                              *Manager
		mockEvents                              *events.MockHandler
		mockHostAPI                             *host.MockAPI
		mockMetric                              *metrics.MockAPI
		ctrl                                    *gomock.Controller
		dbName                                  string
		mockS3Api                               *s3wrapper.MockAPI
	)

	type candidateChecker func()

	mockHostAPIIsRequireUserActionResetFalse := func() {
		mockHostAPI.EXPECT().IsRequireUserActionReset(gomock.Any()).Return(false).AnyTimes()
	}

	checkMasterCandidates := func(times int) candidateChecker {
		return func() {
			mockHostAPI.EXPECT().IsValidMasterCandidate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil).Times(times)
		}
	}

	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		mockHostAPI = host.NewMockAPI(ctrl)
		mockMetric = metrics.NewMockAPI(ctrl)
		operatorsManager := operators.NewManager(common.GetTestLog(), nil, operators.Options{}, nil, nil)
		mockS3Api = s3wrapper.NewMockAPI(ctrl)
		mockS3Api.EXPECT().DoesObjectExist(gomock.Any(), gomock.Any()).Return(false, nil).AnyTimes()
		clusterApi = NewManager(getDefaultConfig(), common.GetTestLog().WithField("pkg", "cluster-monitor"), db,
			mockEvents, mockHostAPI, mockMetric, nil, nil, operatorsManager, nil, mockS3Api, nil)

		hid1 = strfmt.UUID(uuid.New().String())
		hid2 = strfmt.UUID(uuid.New().String())
		hid3 = strfmt.UUID(uuid.New().String())
		hid4 = strfmt.UUID(uuid.New().String())
		hid5 = strfmt.UUID(uuid.New().String())
		clusterId = strfmt.UUID(uuid.New().String())
	})
	Context("All transitions", func() {
		var srcState string
		tests := []struct {
			name               string
			srcState           string
			srcStatusInfo      string
			machineNetworkCidr string
			apiVip             string
			ingressVip         string
			dnsDomain          string
			pullSecretSet      bool
			dstState           string
			hosts              []models.Host
			statusInfoChecker  statusInfoChecker
			validationsChecker *validationsChecker
			candidateChecker   candidateChecker
			errorExpected      bool
		}{
			{
				name:               "pending-for-input to pending-for-input",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: "",
				apiVip:             "",
				ingressVip:         "",
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationPending, messagePattern: "Hosts have not been discovered yet"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationPending, messagePattern: "Machine Network CIDR, API virtual IP, or Ingress virtual IP is undefined"},
					IsApiVipDefined:                     {status: ValidationFailure, messagePattern: "API virtual IP is undefined"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "API virtual IP is undefined"},
					IsIngressVipDefined:                 {status: ValidationFailure, messagePattern: "Ingress virtual IP is undefined"},
					IsIngressVipValid:                   {status: ValidationPending, messagePattern: "Ingress virtual IP is undefined"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
					SufficientMastersCount: {status: ValidationFailure,
						messagePattern: fmt.Sprintf("Clusters must have exactly %d dedicated masters",
							common.MinMasterHostsNeededForInstallation)},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to pending-for-input api-vip-defined",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: "",
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         "",
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationPending, messagePattern: "Hosts have not been discovered yet"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationPending, messagePattern: "Hosts have not been discovered yet"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "Hosts have not been discovered yet"},
					IsIngressVipDefined:                 {status: ValidationFailure, messagePattern: "Ingress virtual IP is undefined"},
					IsIngressVipValid:                   {status: ValidationPending, messagePattern: "Ingress virtual IP is undefined"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
					SufficientMastersCount: {status: ValidationFailure,
						messagePattern: fmt.Sprintf("Clusters must have exactly %d dedicated masters",
							common.MinMasterHostsNeededForInstallation)},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to pending-for-input machine-cidr defined",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             "",
				ingressVip:         "",
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationPending, messagePattern: "Machine Network CIDR, API virtual IP, or Ingress virtual IP is undefined"},
					IsApiVipDefined:                     {status: ValidationFailure, messagePattern: "API virtual IP is undefined"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "API virtual IP is undefined"},
					IsIngressVipDefined:                 {status: ValidationFailure, messagePattern: "Ingress virtual IP is undefined"},
					IsIngressVipValid:                   {status: ValidationPending, messagePattern: "Ingress virtual IP is undefined"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
					SufficientMastersCount: {status: ValidationFailure,
						messagePattern: fmt.Sprintf("Clusters must have exactly %d dedicated masters",
							common.MinMasterHostsNeededForInstallation)},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to pending-for-input api-vip and machine-cidr defined",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         "",
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationPending, messagePattern: "Hosts have not been discovered yet"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "Hosts have not been discovered yet"},
					IsIngressVipDefined:                 {status: ValidationFailure, messagePattern: "Ingress virtual IP is undefined"},
					IsIngressVipValid:                   {status: ValidationPending, messagePattern: "Ingress virtual IP is undefined"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
					SufficientMastersCount: {status: ValidationFailure,
						messagePattern: fmt.Sprintf("Clusters must have exactly %d dedicated masters",
							common.MinMasterHostsNeededForInstallation)},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to pending-for-input with single inventory",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: "",
				apiVip:             "",
				ingressVip:         "",
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster, Inventory: common.GenerateTestDefaultInventory()},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationFailure, messagePattern: "The Machine Network CIDR is undefined; the Machine Network CIDR can be defined by setting either the API or Ingress virtual IPs"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationPending, messagePattern: "Machine Network CIDR, API virtual IP, or Ingress virtual IP is undefined"},
					IsApiVipDefined:                     {status: ValidationFailure, messagePattern: "API virtual IP is undefined"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "API virtual IP is undefined"},
					IsIngressVipDefined:                 {status: ValidationFailure, messagePattern: "Ingress virtual IP is undefined"},
					IsIngressVipValid:                   {status: ValidationPending, messagePattern: "Ingress virtual IP is undefined"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
					SufficientMastersCount: {status: ValidationFailure,
						messagePattern: fmt.Sprintf("Clusters must have exactly %d dedicated masters",
							common.MinMasterHostsNeededForInstallation)},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to pending-for-input with 2 workers",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: "",
				apiVip:             "",
				ingressVip:         "",
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationPending, messagePattern: "Hosts have not been discovered yet"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationPending, messagePattern: "The Machine Network CIDR, API virtual IP, or Ingress virtual IP is undefined."},
					IsApiVipDefined:                     {status: ValidationFailure, messagePattern: "The API virtual IP is undefined"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "The API virtual IP is undefined"},
					IsIngressVipDefined:                 {status: ValidationFailure, messagePattern: "The Ingress virtual IP is undefined"},
					IsIngressVipValid:                   {status: ValidationPending, messagePattern: "The Ingress virtual IP is undefined"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
					SufficientMastersCount: {status: ValidationSuccess,
						messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to pending-for-input with 3 master 2 workers candidates in auto-assign mode",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: "",
				apiVip:             "",
				ingressVip:         "",
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleAutoAssign},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleAutoAssign},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleAutoAssign},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleAutoAssign},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleAutoAssign},
				},
				candidateChecker:  checkMasterCandidates(3),
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationFailure, messagePattern: "The Machine Network CIDR is undefined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationPending, messagePattern: "The Machine Network CIDR, API virtual IP, or Ingress virtual IP is undefined."},
					IsApiVipDefined:                     {status: ValidationFailure, messagePattern: "The API virtual IP is undefined"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "The API virtual IP is undefined"},
					IsIngressVipDefined:                 {status: ValidationFailure, messagePattern: "The Ingress virtual IP is undefined"},
					IsIngressVipValid:                   {status: ValidationPending, messagePattern: "The Ingress virtual IP is undefined"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
					SufficientMastersCount: {status: ValidationSuccess,
						messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to insufficient - masters > 3",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
					SufficientMastersCount: {status: ValidationFailure,
						messagePattern: fmt.Sprintf("Clusters must have exactly %d dedicated masters",
							common.MinMasterHostsNeededForInstallation)},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to insufficient - worker = 1 with auto-assign",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleAutoAssign},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleAutoAssign},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleAutoAssign},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleAutoAssign},
				},
				candidateChecker:  checkMasterCandidates(3),
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
					SufficientMastersCount: {status: ValidationFailure,
						messagePattern: fmt.Sprintf("Clusters must have exactly %d dedicated masters",
							common.MinMasterHostsNeededForInstallation)},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to insufficient - worker = 1",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
					SufficientMastersCount: {status: ValidationFailure,
						messagePattern: fmt.Sprintf("Clusters must have exactly %d dedicated masters",
							common.MinMasterHostsNeededForInstallation)},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to insufficient - not all hosts are ready to install",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInsufficient), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInsufficient), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationFailure, messagePattern: "The cluster has hosts that are not ready to install."},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:               "ready to pending-for-input - api vip not defined",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             "",
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationFailure, messagePattern: "API virtual IP is undefined"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "API virtual IP is undefined"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "ingress vip .* belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:               "ready to pending-for-input - dns domain not defined",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "ingress vip .* belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationFailure, messagePattern: "The base domain is undefined and must be provided"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:               "ready to pending-for-input - pull secret not set",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      false,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "ingress vip .* belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationFailure, messagePattern: "The pull secret is not set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			}, {
				name:               "pending-for-input to ready",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:               "insufficient to ready with disabled master",
				srcState:           models.ClusterStatusInsufficient,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusDisabled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:               "insufficient to ready",
				srcState:           models.ClusterStatusInsufficient,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:               "ready to ready",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:               "finalizing to finalizing",
				srcState:           models.ClusterStatusFinalizing,
				srcStatusInfo:      statusInfoFinalizing,
				dstState:           models.ClusterStatusFinalizing,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker:  makeValueChecker(statusInfoFinalizing),
				validationsChecker: nil,
				errorExpected:      false,
			},
			{
				name:               "error to error",
				srcState:           models.ClusterStatusError,
				dstState:           models.ClusterStatusError,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker:  makeValueChecker(""),
				validationsChecker: nil,
				errorExpected:      false,
			},
			{
				name:               "installed to installed",
				srcState:           models.ClusterStatusInstalled,
				srcStatusInfo:      statusInfoInstalled,
				dstState:           models.ClusterStatusInstalled,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker:  makeValueChecker(statusInfoInstalled),
				validationsChecker: nil,
				errorExpected:      false,
			},
			{
				name:               "Installing to Installing",
				srcState:           models.ClusterStatusInstalling,
				srcStatusInfo:      statusInfoInstalling,
				dstState:           models.ClusterStatusInstalling,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker:  makeValueChecker(statusInfoInstalling),
				validationsChecker: nil,
				errorExpected:      false,
			},
		}

		for i := range tests {
			t := tests[i]
			It(t.name, func() {
				cluster = common.Cluster{
					Cluster: models.Cluster{
						APIVip:                   t.apiVip,
						ID:                       &clusterId,
						IngressVip:               t.ingressVip,
						MachineNetworkCidr:       t.machineNetworkCidr,
						Status:                   &t.srcState,
						StatusInfo:               &t.srcStatusInfo,
						BaseDNSDomain:            t.dnsDomain,
						PullSecretSet:            t.pullSecretSet,
						ClusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
						ClusterNetworkHostPrefix: common.TestIPv4Networking.ClusterNetworkHostPrefix,
						ServiceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
						NetworkType:              swag.String(models.ClusterNetworkTypeOVNKubernetes),
					},
				}
				Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
				for i := range t.hosts {
					t.hosts[i].InfraEnvID = clusterId
					t.hosts[i].ClusterID = &clusterId
					Expect(db.Create(&t.hosts[i]).Error).ShouldNot(HaveOccurred())
				}
				cluster = getClusterFromDB(clusterId, db)
				if srcState != t.dstState {
					mockEvents.EXPECT().AddEvent(gomock.Any(), gomock.Any(), gomock.Any(),
						gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				}
				if t.dstState == models.ClusterStatusInsufficient {
					mockHostAPIIsRequireUserActionResetFalse()
				}
				if t.candidateChecker != nil {
					t.candidateChecker()
				}
				Expect(cluster.ValidationsInfo).To(BeEmpty())
				clusterAfterRefresh, err := clusterApi.RefreshStatus(ctx, &cluster, db)
				if t.errorExpected {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).ToNot(HaveOccurred())
				}
				Expect(clusterAfterRefresh.Status).To(Equal(&t.dstState))
				t.statusInfoChecker.check(clusterAfterRefresh.StatusInfo)
				if t.validationsChecker != nil {
					t.validationsChecker.check(clusterAfterRefresh.ValidationsInfo)
					Expect(clusterAfterRefresh.ValidationsInfo).ToNot(BeEmpty())
				} else {
					Expect(clusterAfterRefresh.ValidationsInfo).To(BeEmpty())
				}
			})
		}
	})
	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
		ctrl.Finish()
	})
})

var _ = Describe("RefreshCluster - preparing for install", func() {
	var (
		ctx                                     = context.Background()
		db                                      *gorm.DB
		clusterId, hid1, hid2, hid3, hid4, hid5 strfmt.UUID
		cluster                                 common.Cluster
		clusterApi                              *Manager
		mockEvents                              *events.MockHandler
		mockHostAPI                             *host.MockAPI
		mockMetric                              *metrics.MockAPI
		ctrl                                    *gomock.Controller
		dbName                                  string
	)

	mockHostAPIIsRequireUserActionResetFalse := func() {
		mockHostAPI.EXPECT().IsRequireUserActionReset(gomock.Any()).Return(false).AnyTimes()
	}
	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		mockHostAPI = host.NewMockAPI(ctrl)
		mockMetric = metrics.NewMockAPI(ctrl)
		operatorsManager := operators.NewManager(common.GetTestLog(), nil, operators.Options{}, nil, nil)
		dnsApi := dns.NewDNSHandler(nil, common.GetTestLog())
		clusterApi = NewManager(getDefaultConfig(), common.GetTestLog().WithField("pkg", "cluster-monitor"), db,
			mockEvents, mockHostAPI, mockMetric, nil, nil, operatorsManager, nil, nil, dnsApi)

		mockHostAPI.EXPECT().IsValidMasterCandidate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil).AnyTimes()
		hid1 = strfmt.UUID(uuid.New().String())
		hid2 = strfmt.UUID(uuid.New().String())
		hid3 = strfmt.UUID(uuid.New().String())
		hid4 = strfmt.UUID(uuid.New().String())
		hid5 = strfmt.UUID(uuid.New().String())
		clusterId = strfmt.UUID(uuid.New().String())
	})
	tests := []struct {
		name               string
		apiVip             string
		ingressVip         string
		dstState           string
		installationStatus string
		hosts              []models.Host
		statusInfoChecker  statusInfoChecker
		validationsChecker *validationsChecker
	}{
		{
			name:       "no change",
			apiVip:     common.TestIPv4Networking.APIVip,
			ingressVip: common.TestIPv4Networking.IngressVip,
			dstState:   models.ClusterStatusPreparingForInstallation,
			hosts: []models.Host{
				{
					ID:     &hid1,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
				{
					ID:     &hid2,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
				{
					ID:     &hid3,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
			},
			statusInfoChecker: makeValueChecker(statusInfoPreparingForInstallation),
		},
		{
			name:       "no change - all hosts prepared + disabled",
			apiVip:     common.TestIPv4Networking.APIVip,
			ingressVip: common.TestIPv4Networking.IngressVip,
			dstState:   models.ClusterStatusPreparingForInstallation,
			hosts: []models.Host{
				{
					ID:     &hid1,
					Status: swag.String(models.HostStatusPreparingSuccessful),
				},
				{
					ID:     &hid2,
					Status: swag.String(models.HostStatusPreparingSuccessful),
				},
				{
					ID:     &hid3,
					Status: swag.String(models.HostStatusPreparingSuccessful),
				},
				{
					ID:     &hid4,
					Status: swag.String(models.HostStatusDisabled),
				},
			},
			statusInfoChecker: makeValueChecker(statusInfoPreparingForInstallation),
		},
		{
			name:       "one insufficient host",
			apiVip:     common.TestIPv4Networking.APIVip,
			ingressVip: common.TestIPv4Networking.IngressVip,
			dstState:   models.ClusterStatusInsufficient,
			hosts: []models.Host{
				{
					ID:     &hid1,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
				{
					ID:     &hid2,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
				{
					ID:     &hid3,
					Status: swag.String(models.HostStatusInsufficient),
				},
			},
			statusInfoChecker: makeValueChecker(statusInfoUnpreparingHostExists),
		},
		{
			name:       "one insufficient host + preparation failed",
			apiVip:     common.TestIPv4Networking.APIVip,
			ingressVip: common.TestIPv4Networking.IngressVip,
			dstState:   models.ClusterStatusInsufficient,
			hosts: []models.Host{
				{
					ID:     &hid1,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
				{
					ID:     &hid2,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
				{
					ID:     &hid3,
					Status: swag.String(models.HostStatusInsufficient),
				},
			},
			installationStatus: common.InstallationPreparationFailed,
			statusInfoChecker:  makeValueChecker(statusInfoUnpreparingHostExists),
		},
		{
			name:       "one insufficient host + preparation succeeded",
			apiVip:     common.TestIPv4Networking.APIVip,
			ingressVip: common.TestIPv4Networking.IngressVip,
			dstState:   models.ClusterStatusInsufficient,
			hosts: []models.Host{
				{
					ID:     &hid1,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
				{
					ID:     &hid2,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
				{
					ID:     &hid3,
					Status: swag.String(models.HostStatusInsufficient),
				},
			},
			installationStatus: common.InstallationPreparationSucceeded,
			statusInfoChecker:  makeValueChecker(statusInfoUnpreparingHostExists),
		},
		{
			name:       "preparation failed",
			apiVip:     common.TestIPv4Networking.APIVip,
			ingressVip: common.TestIPv4Networking.IngressVip,
			dstState:   models.ClusterStatusReady,
			hosts: []models.Host{
				{
					ID:     &hid1,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
				{
					ID:     &hid2,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
				{
					ID:     &hid3,
					Status: swag.String(models.HostStatusPreparingForInstallation),
				},
			},
			installationStatus: common.InstallationPreparationFailed,
			statusInfoChecker:  makeValueChecker(statusInfoClusterFailedToPrepare),
		},
		{
			name:       "all hosts prepared + preparation succeeded",
			apiVip:     common.TestIPv4Networking.APIVip,
			ingressVip: common.TestIPv4Networking.IngressVip,
			dstState:   models.ClusterStatusInstalling,
			hosts: []models.Host{
				{
					ID:     &hid1,
					Status: swag.String(models.HostStatusPreparingSuccessful),
				},
				{
					ID:     &hid2,
					Status: swag.String(models.HostStatusPreparingSuccessful),
				},
				{
					ID:     &hid3,
					Status: swag.String(models.HostStatusPreparingSuccessful),
				},
			},
			installationStatus: common.InstallationPreparationSucceeded,
			statusInfoChecker:  makeValueChecker(statusInfoInstalling),
		},
		{
			name:       "all hosts prepared + preparation succeeded + disabled",
			apiVip:     common.TestIPv4Networking.APIVip,
			ingressVip: common.TestIPv4Networking.IngressVip,
			dstState:   models.ClusterStatusInstalling,
			hosts: []models.Host{
				{
					ID:     &hid1,
					Status: swag.String(models.HostStatusPreparingSuccessful),
				},
				{
					ID:     &hid2,
					Status: swag.String(models.HostStatusPreparingSuccessful),
				},
				{
					ID:     &hid3,
					Status: swag.String(models.HostStatusPreparingSuccessful),
				},
				{
					ID:     &hid4,
					Status: swag.String(models.HostStatusDisabled),
				},
				{
					ID:     &hid5,
					Status: swag.String(models.HostStatusDisabled),
				},
			},
			installationStatus: common.InstallationPreparationSucceeded,
			statusInfoChecker:  makeValueChecker(statusInfoInstalling),
		},
	}
	for i := range tests {
		t := tests[i]
		It(t.name, func() {
			cluster = common.Cluster{
				Cluster: models.Cluster{
					APIVip:          t.apiVip,
					ID:              &clusterId,
					IngressVip:      t.ingressVip,
					PullSecretSet:   true,
					BaseDNSDomain:   "test.com",
					Status:          swag.String(models.ClusterStatusPreparingForInstallation),
					StatusInfo:      swag.String(statusInfoPreparingForInstallation),
					StatusUpdatedAt: strfmt.DateTime(time.Now()),
				},
				InstallationPreparationCompletionStatus: t.installationStatus,
			}
			Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
			for i := range t.hosts {
				t.hosts[i].InfraEnvID = clusterId
				t.hosts[i].ClusterID = &clusterId
				Expect(db.Create(&t.hosts[i]).Error).ShouldNot(HaveOccurred())
			}
			cluster = getClusterFromDB(clusterId, db)
			if t.dstState != models.ClusterStatusPreparingForInstallation {
				mockEvents.EXPECT().AddEvent(gomock.Any(), gomock.Any(), gomock.Any(),
					gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			}
			switch t.dstState {
			case models.ClusterStatusInsufficient:
				mockHostAPIIsRequireUserActionResetFalse()
			case models.ClusterStatusInstalling:
				nonDisabled := 0
				for _, h := range t.hosts {
					if swag.StringValue(h.Status) != models.HostStatusDisabled {
						nonDisabled++
					}
				}
				mockMetric.EXPECT().InstallationStarted(gomock.Any(), clusterId, gomock.Any(), gomock.Any()).Times(1)
				mockMetric.EXPECT().ClusterHostInstallationCount(gomock.Any(), nonDisabled, gomock.Any()).Times(1)
			}
			Expect(cluster.ValidationsInfo).To(BeEmpty())
			clusterAfterRefresh, err := clusterApi.RefreshStatus(ctx, &cluster, db)
			Expect(err).ToNot(HaveOccurred())
			Expect(clusterAfterRefresh.Status).To(Equal(&t.dstState))
			t.statusInfoChecker.check(clusterAfterRefresh.StatusInfo)
			if t.validationsChecker != nil {
				t.validationsChecker.check(clusterAfterRefresh.ValidationsInfo)
				Expect(clusterAfterRefresh.ValidationsInfo).ToNot(BeEmpty())
			}
		})
	}
	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
		ctrl.Finish()
	})
})

var _ = Describe("Refresh Cluster - Advanced networking validations", func() {
	var (
		ctx                                     = context.Background()
		db                                      *gorm.DB
		clusterId, hid1, hid2, hid3, hid4, hid5 strfmt.UUID
		cluster                                 common.Cluster
		clusterApi                              *Manager
		mockEvents                              *events.MockHandler
		mockHostAPI                             *host.MockAPI
		mockMetric                              *metrics.MockAPI
		ctrl                                    *gomock.Controller
		dbName                                  string
	)

	mockHostAPIIsRequireUserActionResetFalse := func() {
		mockHostAPI.EXPECT().IsRequireUserActionReset(gomock.Any()).Return(false).AnyTimes()
	}
	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		mockHostAPI = host.NewMockAPI(ctrl)
		mockMetric = metrics.NewMockAPI(ctrl)
		operatorsManager := operators.NewManager(common.GetTestLog(), nil, operators.Options{}, nil, nil)
		clusterApi = NewManager(getDefaultConfig(), common.GetTestLog().WithField("pkg", "cluster-monitor"), db,
			mockEvents, mockHostAPI, mockMetric, nil, nil, operatorsManager, nil, nil, nil)

		hid1 = strfmt.UUID(uuid.New().String())
		hid2 = strfmt.UUID(uuid.New().String())
		hid3 = strfmt.UUID(uuid.New().String())
		hid4 = strfmt.UUID(uuid.New().String())
		hid5 = strfmt.UUID(uuid.New().String())
		clusterId = strfmt.UUID(uuid.New().String())
	})
	Context("All transitions", func() {
		var srcState string
		tests := []struct {
			name                     string
			srcState                 string
			srcStatusInfo            string
			machineNetworkCidr       string
			clusterNetworkCidr       string
			serviceNetworkCidr       string
			clusterNetworkHostPrefix int64
			apiVip                   string
			ingressVip               string
			dstState                 string
			hosts                    []models.Host
			statusInfoChecker        statusInfoChecker
			validationsChecker       *validationsChecker
			errorExpected            bool
			userManagedNetworking    bool
			networkType              string
		}{
			{
				name:               "pending-for-input to pending-for-input",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationFailure, messagePattern: "Cluster Network CIDR is undefined"},
					isServiceCidrDefined:                {status: ValidationFailure, messagePattern: "Service Network CIDR is undefined"},
					noCidrOverlapping:                   {status: ValidationPending, messagePattern: "At least one of the CIDRs .Machine Network, Cluster Network, Service Network. is undefined"},
					networkPrefixValid:                  {status: ValidationPending, messagePattern: "Cluster Network CIDR is undefined"},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to pending-for-input user-managed-networking",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: "",
				apiVip:             "",
				ingressVip:         "",
				networkType:        "",
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "No Machine Network CIDR needed: User Managed Networking"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is not required: User Managed Networking"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is not required: User Managed Networking"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "The API virtual IP is not required: User Managed Networking"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is not required: User Managed Networking"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is not required: User Managed Networking"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationFailure, messagePattern: "Cluster Network CIDR is undefined"},
					isServiceCidrDefined:                {status: ValidationFailure, messagePattern: "Service Network CIDR is undefined"},
					noCidrOverlapping:                   {status: ValidationPending, messagePattern: "At least one of the CIDRs .Cluster Network, Service Network. is undefined"},
					networkPrefixValid:                  {status: ValidationPending, messagePattern: "Cluster Network CIDR is undefined"},
					isNetworkTypeValid:                  {status: ValidationFailure, messagePattern: "The network type is not valid; the valid network types are OpenShiftSDN or OVNKubernetes"},
				}),
				errorExpected:         false,
				userManagedNetworking: true,
			},
			{
				name:                     "pending-for-input to ready user-managed-networking",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusReady,
				clusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv4Networking.ClusterNetworkHostPrefix,
				serviceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv4Networking.APIVip,
				ingressVip:               common.TestIPv4Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationSuccess, messagePattern: "Cluster Network prefix is valid."},
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "No Machine Network CIDR needed: User Managed Networking"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is not required: User Managed Networking"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is not required: User Managed Networking"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "The API virtual IP is not required: User Managed Networking"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is not required: User Managed Networking"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is not required: User Managed Networking"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected:         false,
				userManagedNetworking: true,
			},
			{
				name:               "pending-for-input to insufficient - overlapping",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusInsufficient,
				clusterNetworkCidr: common.TestIPv4Networking.ClusterNetworkCidr,
				serviceNetworkCidr: common.IncrementCidrMask(common.TestIPv4Networking.MachineNetworkCidr),
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				networkType:        models.ClusterNetworkTypeOpenShiftSDN,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationFailure, messagePattern: "MachineNetworkCIDR and ServiceNetworkCIDR: CIDRS .* and .* overlap"},
					networkPrefixValid:                  {status: ValidationFailure, messagePattern: "Host prefix, now 0, must be a positive integer"},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to insufficient - prefix too low",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusInsufficient,
				clusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: 1,
				serviceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv4Networking.APIVip,
				ingressVip:               common.TestIPv4Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationFailure, messagePattern: "Cluster network CIDR prefix .* does not contain enough addresses for 5 hosts each one with 1 prefix"},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to insufficient - prefix too high",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusInsufficient,
				clusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv4Networking.ClusterNetworkHostPrefix * 2,
				serviceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv4Networking.APIVip,
				ingressVip:               common.TestIPv4Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid: {status: ValidationFailure, messagePattern: fmt.Sprintf(
						"Host prefix, now %d, must be less than or equal to 25 to allow at least 128 addresses", common.TestIPv4Networking.ClusterNetworkHostPrefix*2)},
					isNetworkTypeValid: {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to ready",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusReady,
				clusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv4Networking.ClusterNetworkHostPrefix,
				serviceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv4Networking.APIVip,
				ingressVip:               common.TestIPv4Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationSuccess, messagePattern: "Cluster Network prefix is valid."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to insufficient - not enough addresses",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusInsufficient,
				clusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv4Networking.ClusterNetworkHostPrefix / 2,
				serviceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv4Networking.APIVip,
				ingressVip:               common.TestIPv4Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationFailure, messagePattern: "does not contain enough addresses for"},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to ready - with workers",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusReady,
				clusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv4Networking.ClusterNetworkHostPrefix,
				serviceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv4Networking.APIVip,
				ingressVip:               common.TestIPv4Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationSuccess, messagePattern: "Cluster Network prefix is valid."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
		}

		for i := range tests {
			t := tests[i]
			It(t.name, func() {
				cluster = common.Cluster{
					Cluster: models.Cluster{
						APIVip:                   t.apiVip,
						ID:                       &clusterId,
						IngressVip:               t.ingressVip,
						MachineNetworkCidr:       t.machineNetworkCidr,
						Status:                   &t.srcState,
						StatusInfo:               &t.srcStatusInfo,
						ClusterNetworkCidr:       t.clusterNetworkCidr,
						ServiceNetworkCidr:       t.serviceNetworkCidr,
						ClusterNetworkHostPrefix: t.clusterNetworkHostPrefix,
						PullSecretSet:            true,
						BaseDNSDomain:            "test.com",
						UserManagedNetworking:    &t.userManagedNetworking,
						NetworkType:              &t.networkType,
					},
				}
				Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
				for i := range t.hosts {
					t.hosts[i].InfraEnvID = clusterId
					t.hosts[i].ClusterID = &clusterId
					Expect(db.Create(&t.hosts[i]).Error).ShouldNot(HaveOccurred())
				}
				cluster = getClusterFromDB(clusterId, db)
				if srcState != t.dstState {
					mockEvents.EXPECT().AddEvent(gomock.Any(), gomock.Any(), gomock.Any(),
						gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				}
				if t.dstState == models.ClusterStatusInsufficient {
					mockHostAPIIsRequireUserActionResetFalse()
				}
				Expect(cluster.ValidationsInfo).To(BeEmpty())
				clusterAfterRefresh, err := clusterApi.RefreshStatus(ctx, &cluster, db)
				if t.errorExpected {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).ToNot(HaveOccurred())
				}
				Expect(clusterAfterRefresh.Status).To(Equal(&t.dstState))
				t.statusInfoChecker.check(clusterAfterRefresh.StatusInfo)
				if t.validationsChecker != nil {
					t.validationsChecker.check(clusterAfterRefresh.ValidationsInfo)
					Expect(clusterAfterRefresh.ValidationsInfo).ToNot(BeEmpty())
				} else {
					Expect(clusterAfterRefresh.ValidationsInfo).To(BeEmpty())
				}
			})
		}
	})
	Context("All transitions IPv6", func() {
		var srcState string
		tests := []struct {
			name                     string
			srcState                 string
			srcStatusInfo            string
			machineNetworkCidr       string
			clusterNetworkCidr       string
			serviceNetworkCidr       string
			clusterNetworkHostPrefix int64
			apiVip                   string
			ingressVip               string
			dstState                 string
			hosts                    []models.Host
			statusInfoChecker        statusInfoChecker
			validationsChecker       *validationsChecker
			errorExpected            bool
			userManagedNetworking    bool
			networkType              string
		}{
			{
				name:               "pending-for-input to pending-for-input",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: common.TestIPv6Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv6Networking.APIVip,
				ingressVip:         common.TestIPv6Networking.IngressVip,
				networkType:        models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationFailure, messagePattern: "Cluster Network CIDR is undefined"},
					isServiceCidrDefined:                {status: ValidationFailure, messagePattern: "Service Network CIDR is undefined"},
					noCidrOverlapping:                   {status: ValidationPending, messagePattern: "At least one of the CIDRs .Machine Network, Cluster Network, Service Network. is undefined"},
					networkPrefixValid:                  {status: ValidationPending, messagePattern: "Cluster Network CIDR is undefined"},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to pending-for-input user-managed-networking",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: "",
				apiVip:             "",
				ingressVip:         "",
				networkType:        "bla",
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "No Machine Network CIDR needed: User Managed Networking"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is not required: User Managed Networking"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is not required: User Managed Networking"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "The API virtual IP is not required: User Managed Networking"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is not required: User Managed Networking"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is not required: User Managed Networking"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationFailure, messagePattern: "Cluster Network CIDR is undefined"},
					isServiceCidrDefined:                {status: ValidationFailure, messagePattern: "Service Network CIDR is undefined"},
					noCidrOverlapping:                   {status: ValidationPending, messagePattern: "At least one of the CIDRs .Cluster Network, Service Network. is undefined"},
					networkPrefixValid:                  {status: ValidationPending, messagePattern: "Cluster Network CIDR is undefined"},
					isNetworkTypeValid:                  {status: ValidationFailure, messagePattern: "The network type is not valid; the valid network types are OpenShiftSDN or OVNKubernetes"}}),
				errorExpected:         false,
				userManagedNetworking: true,
			},
			{
				name:               "pending-for-input to pending-for-input user-managed-networking",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: "",
				apiVip:             "",
				ingressVip:         "",
				networkType:        "bla",
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "No Machine Network CIDR needed: User Managed Networking"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is not required: User Managed Networking"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is not required: User Managed Networking"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "The API virtual IP is not required: User Managed Networking"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is not required: User Managed Networking"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is not required: User Managed Networking"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationFailure, messagePattern: "Cluster Network CIDR is undefined"},
					isServiceCidrDefined:                {status: ValidationFailure, messagePattern: "Service Network CIDR is undefined"},
					noCidrOverlapping:                   {status: ValidationPending, messagePattern: "At least one of the CIDRs .Cluster Network, Service Network. is undefined"},
					networkPrefixValid:                  {status: ValidationPending, messagePattern: "Cluster Network CIDR is undefined"},
					isNetworkTypeValid:                  {status: ValidationFailure, messagePattern: "The network type is not valid; the valid network types are OpenShiftSDN or OVNKubernetes"}}),
				errorExpected:         false,
				userManagedNetworking: true,
			},
			{
				name:                     "pending-for-input to ready user-managed-networking testing_now",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusReady,
				clusterNetworkCidr:       common.TestIPv6Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv6Networking.ClusterNetworkHostPrefix,
				serviceNetworkCidr:       common.TestIPv6Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv6Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv6Networking.APIVip,
				ingressVip:               common.TestIPv6Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationSuccess, messagePattern: "Cluster Network prefix is valid."},
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "No Machine Network CIDR needed: User Managed Networking"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is not required: User Managed Networking"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is not required: User Managed Networking"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "The API virtual IP is not required: User Managed Networking"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is not required: User Managed Networking"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is not required: User Managed Networking"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected:         false,
				userManagedNetworking: true,
			},
			{
				name:               "pending-for-input to insufficient - overlapping",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusInsufficient,
				clusterNetworkCidr: common.TestIPv6Networking.ClusterNetworkCidr,
				serviceNetworkCidr: common.IncrementCidrMask(common.TestIPv6Networking.MachineNetworkCidr),
				machineNetworkCidr: common.TestIPv6Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv6Networking.APIVip,
				ingressVip:         common.TestIPv6Networking.IngressVip,
				networkType:        models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationFailure, messagePattern: "MachineNetworkCIDR and ServiceNetworkCIDR: CIDRS .* and .* overlap"},
					networkPrefixValid:                  {status: ValidationFailure, messagePattern: "Host prefix, now 0, must be a positive integer"},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to insufficient - prefix too low",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusInsufficient,
				clusterNetworkCidr:       common.TestIPv6Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: 1,
				serviceNetworkCidr:       common.TestIPv6Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv6Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv6Networking.APIVip,
				ingressVip:               common.TestIPv6Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationFailure, messagePattern: "Cluster network CIDR prefix .* does not contain enough addresses for 5 hosts each one with 1 prefix"},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to insufficient networkType invalid",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusInsufficient,
				clusterNetworkCidr:       common.TestIPv6Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv6Networking.ClusterNetworkHostPrefix,
				serviceNetworkCidr:       common.TestIPv6Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv6Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv6Networking.APIVip,
				ingressVip:               common.TestIPv6Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOpenShiftSDN,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
				},

				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationSuccess, messagePattern: "Cluster Network prefix is valid."},
					isNetworkTypeValid:                  {status: ValidationFailure, messagePattern: "The cluster is configured with IPv6 which is not supported by OpenShiftSDN; use OVNKubernetes instead"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to insufficient - prefix too high",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusInsufficient,
				clusterNetworkCidr:       common.TestIPv6Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv6Networking.ClusterNetworkHostPrefix * 2,
				serviceNetworkCidr:       common.TestIPv6Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv6Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv6Networking.APIVip,
				ingressVip:               common.TestIPv6Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid: {status: ValidationFailure, messagePattern: fmt.Sprintf(
						"Host prefix, now %d, must be less than or equal to 121 to allow at least 128 addresses", common.TestIPv6Networking.ClusterNetworkHostPrefix*2)},
					isNetworkTypeValid: {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to ready",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusReady,
				clusterNetworkCidr:       common.TestIPv6Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv6Networking.ClusterNetworkHostPrefix,
				serviceNetworkCidr:       common.TestIPv6Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv6Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv6Networking.APIVip,
				ingressVip:               common.TestIPv6Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationSuccess, messagePattern: "Cluster Network prefix is valid."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to insufficient - not enough addresses",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusInsufficient,
				clusterNetworkCidr:       common.TestIPv6Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv6Networking.ClusterNetworkHostPrefix / 2,
				serviceNetworkCidr:       common.TestIPv6Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv6Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv6Networking.APIVip,
				ingressVip:               common.TestIPv6Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationFailure, messagePattern: "does not contain enough addresses for"},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
			{
				name:                     "pending-for-input to ready",
				srcState:                 models.ClusterStatusPendingForInput,
				dstState:                 models.ClusterStatusReady,
				clusterNetworkCidr:       common.TestIPv6Networking.ClusterNetworkCidr,
				clusterNetworkHostPrefix: common.TestIPv6Networking.ClusterNetworkHostPrefix,
				serviceNetworkCidr:       common.TestIPv6Networking.ServiceNetworkCidr,
				machineNetworkCidr:       common.TestIPv6Networking.MachineNetworkCidr,
				apiVip:                   common.TestIPv6Networking.APIVip,
				ingressVip:               common.TestIPv6Networking.IngressVip,
				networkType:              models.ClusterNetworkTypeOVNKubernetes,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					isClusterCidrDefined:                {status: ValidationSuccess, messagePattern: "Cluster Network CIDR is defined"},
					isServiceCidrDefined:                {status: ValidationSuccess, messagePattern: "Service Network CIDR is defined"},
					noCidrOverlapping:                   {status: ValidationSuccess, messagePattern: "No CIDRS are overlapping"},
					networkPrefixValid:                  {status: ValidationSuccess, messagePattern: "Cluster Network prefix is valid."},
					isNetworkTypeValid:                  {status: ValidationSuccess, messagePattern: "The cluster has a valid network type"},
				}),
				errorExpected: false,
			},
		}

		for i := range tests {
			t := tests[i]
			It(t.name, func() {
				cluster = common.Cluster{
					Cluster: models.Cluster{
						APIVip:                   t.apiVip,
						ID:                       &clusterId,
						IngressVip:               t.ingressVip,
						MachineNetworkCidr:       t.machineNetworkCidr,
						Status:                   &t.srcState,
						StatusInfo:               &t.srcStatusInfo,
						ClusterNetworkCidr:       t.clusterNetworkCidr,
						ServiceNetworkCidr:       t.serviceNetworkCidr,
						ClusterNetworkHostPrefix: t.clusterNetworkHostPrefix,
						PullSecretSet:            true,
						BaseDNSDomain:            "test.com",
						UserManagedNetworking:    &t.userManagedNetworking,
						NetworkType:              &t.networkType,
					},
				}
				Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
				for i := range t.hosts {
					t.hosts[i].InfraEnvID = clusterId
					t.hosts[i].ClusterID = &clusterId
					Expect(db.Create(&t.hosts[i]).Error).ShouldNot(HaveOccurred())
				}
				cluster = getClusterFromDB(clusterId, db)
				if srcState != t.dstState {
					mockEvents.EXPECT().AddEvent(gomock.Any(), gomock.Any(), gomock.Any(),
						gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				}
				if t.dstState == models.ClusterStatusInsufficient {
					mockHostAPIIsRequireUserActionResetFalse()
				}
				Expect(cluster.ValidationsInfo).To(BeEmpty())
				clusterAfterRefresh, err := clusterApi.RefreshStatus(ctx, &cluster, db)
				if t.errorExpected {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).ToNot(HaveOccurred())
				}
				Expect(clusterAfterRefresh.Status).To(Equal(&t.dstState))
				t.statusInfoChecker.check(clusterAfterRefresh.StatusInfo)
				if t.validationsChecker != nil {
					t.validationsChecker.check(clusterAfterRefresh.ValidationsInfo)
					Expect(clusterAfterRefresh.ValidationsInfo).ToNot(BeEmpty())
				} else {
					Expect(clusterAfterRefresh.ValidationsInfo).To(BeEmpty())
				}

			})
		}
	})
	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
		ctrl.Finish()
	})
})

var _ = Describe("Refresh Cluster - With DHCP", func() {
	var (
		ctx                                     = context.Background()
		db                                      *gorm.DB
		clusterId, hid1, hid2, hid3, hid4, hid5 strfmt.UUID
		cluster                                 common.Cluster
		clusterApi                              *Manager
		mockEvents                              *events.MockHandler
		mockHostAPI                             *host.MockAPI
		mockMetric                              *metrics.MockAPI
		ctrl                                    *gomock.Controller
		dbName                                  string
		mockS3Api                               *s3wrapper.MockAPI
	)

	mockHostAPIIsRequireUserActionResetFalse := func() {
		mockHostAPI.EXPECT().IsRequireUserActionReset(gomock.Any()).Return(false).AnyTimes()
	}
	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		mockHostAPI = host.NewMockAPI(ctrl)
		mockMetric = metrics.NewMockAPI(ctrl)
		operatorsManager := operators.NewManager(common.GetTestLog(), nil, operators.Options{}, nil, nil)
		mockS3Api = s3wrapper.NewMockAPI(ctrl)
		mockS3Api.EXPECT().DoesObjectExist(gomock.Any(), gomock.Any()).Return(false, nil).AnyTimes()
		clusterApi = NewManager(getDefaultConfig(), common.GetTestLog().WithField("pkg", "cluster-monitor"), db,
			mockEvents, mockHostAPI, mockMetric, nil, nil, operatorsManager, nil, mockS3Api, nil)

		hid1 = strfmt.UUID(uuid.New().String())
		hid2 = strfmt.UUID(uuid.New().String())
		hid3 = strfmt.UUID(uuid.New().String())
		hid4 = strfmt.UUID(uuid.New().String())
		hid5 = strfmt.UUID(uuid.New().String())
		clusterId = strfmt.UUID(uuid.New().String())
	})
	Context("All transitions", func() {
		var srcState string
		tests := []struct {
			name                    string
			srcState                string
			srcStatusInfo           string
			machineNetworkCidr      string
			apiVip                  string
			ingressVip              string
			dnsDomain               string
			pullSecretSet           bool
			dstState                string
			hosts                   []models.Host
			statusInfoChecker       statusInfoChecker
			validationsChecker      *validationsChecker
			setMachineCidrUpdatedAt bool
			errorExpected           bool
		}{
			{
				name:               "pending-for-input to pending-for-input",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusPendingForInput,
				machineNetworkCidr: "",
				apiVip:             "",
				ingressVip:         "",
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(statusInfoPendingForInput),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationPending, messagePattern: "Hosts have not been discovered yet"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationPending, messagePattern: "Machine Network CIDR, API virtual IP, or Ingress virtual IP is undefined"},
					IsApiVipDefined:                     {status: ValidationPending, messagePattern: "Machine Network CIDR is undefined"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "API virtual IP is undefined"},
					IsIngressVipDefined:                 {status: ValidationPending, messagePattern: "Machine Network CIDR is undefined"},
					IsIngressVipValid:                   {status: ValidationPending, messagePattern: "Ingress virtual IP is undefined"},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount: {status: ValidationFailure,
						messagePattern: fmt.Sprintf("Clusters must have exactly %d dedicated masters",
							common.MinMasterHostsNeededForInstallation)},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to insufficient - masters > 3",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount: {status: ValidationFailure,
						messagePattern: fmt.Sprintf("Clusters must have exactly %d dedicated masters",
							common.MinMasterHostsNeededForInstallation)},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to insufficient - not all hosts are ready to install - not enough workers",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInsufficient), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationFailure, messagePattern: "The cluster has hosts that are not ready to install."},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationFailure, messagePattern: "Clusters must have exactly 3 dedicated masters"},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to insufficient - not all hosts are ready to install",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInsufficient), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInsufficient), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInsufficient), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationFailure, messagePattern: "The cluster has hosts that are not ready to install."},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},
			{
				name:               "ready to dhcp timeout - api vip not defined",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             "",
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationFailure, messagePattern: "API virtual IP is undefined; IP allocation from the DHCP server timed out"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "API virtual IP is undefined"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "ingress vip .* belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},
			{
				name:               "ready to insufficient - api vip not defined",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             "",
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationFailure, messagePattern: "API virtual IP is undefined; after the Machine Network CIDR has been defined, the API virtual IP is received from a DHCP lease allocation task which may take up to 2 minutes"},
					IsApiVipValid:                       {status: ValidationPending, messagePattern: "API virtual IP is undefined"},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "ingress vip .* belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				setMachineCidrUpdatedAt: true,
				errorExpected:           false,
			},
			{
				name:               "dhcp timeout to ready",
				srcState:           models.ClusterStatusInsufficient,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "ingress vip .* belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to ready",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},
			{
				name:               "insufficient to ready",
				srcState:           models.ClusterStatusInsufficient,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},
			{
				name:               "ready to ready",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},
			{
				name:               "finalizing to finalizing",
				srcState:           models.ClusterStatusFinalizing,
				srcStatusInfo:      statusInfoFinalizing,
				dstState:           models.ClusterStatusFinalizing,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker:  makeValueChecker(statusInfoFinalizing),
				validationsChecker: nil,
				errorExpected:      false,
			},
			{
				name:               "error to error",
				srcState:           models.ClusterStatusError,
				dstState:           models.ClusterStatusError,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker:  makeValueChecker(""),
				validationsChecker: nil,
				errorExpected:      false,
			},
			{
				name:               "installed to installed",
				srcState:           models.ClusterStatusInstalled,
				srcStatusInfo:      statusInfoInstalled,
				dstState:           models.ClusterStatusInstalled,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusKnown), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker:  makeValueChecker(statusInfoInstalled),
				validationsChecker: nil,
				errorExpected:      false,
			},
			{
				name:               "Installing to Installing",
				srcState:           models.ClusterStatusInstalling,
				srcStatusInfo:      statusInfoInstalling,
				dstState:           models.ClusterStatusInstalling,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker:  makeValueChecker(statusInfoInstalling),
				validationsChecker: nil,
				errorExpected:      false,
			},
		}

		for i := range tests {
			t := tests[i]
			It(t.name, func() {
				cluster = common.Cluster{
					Cluster: models.Cluster{
						APIVip:                   t.apiVip,
						ID:                       &clusterId,
						IngressVip:               t.ingressVip,
						MachineNetworkCidr:       t.machineNetworkCidr,
						Status:                   &t.srcState,
						StatusInfo:               &t.srcStatusInfo,
						VipDhcpAllocation:        swag.Bool(true),
						BaseDNSDomain:            t.dnsDomain,
						PullSecretSet:            t.pullSecretSet,
						ClusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
						ClusterNetworkHostPrefix: common.TestIPv4Networking.ClusterNetworkHostPrefix,
						ServiceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
						NetworkType:              swag.String(models.ClusterNetworkTypeOVNKubernetes),
					},
				}
				if t.setMachineCidrUpdatedAt {
					cluster.MachineNetworkCidrUpdatedAt = time.Now()
				} else {
					cluster.MachineNetworkCidrUpdatedAt = time.Now().Add(-3 * time.Minute)
				}
				Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
				for i := range t.hosts {
					t.hosts[i].InfraEnvID = clusterId
					t.hosts[i].ClusterID = &clusterId
					Expect(db.Create(&t.hosts[i]).Error).ShouldNot(HaveOccurred())
				}
				cluster = getClusterFromDB(clusterId, db)
				if srcState != t.dstState {
					mockEvents.EXPECT().AddEvent(gomock.Any(), gomock.Any(), gomock.Any(),
						gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				}
				if t.dstState == models.ClusterStatusInsufficient {
					mockHostAPIIsRequireUserActionResetFalse()
				}

				Expect(cluster.ValidationsInfo).To(BeEmpty())
				clusterAfterRefresh, err := clusterApi.RefreshStatus(ctx, &cluster, db)
				if t.errorExpected {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).ToNot(HaveOccurred())
				}
				Expect(clusterAfterRefresh.Status).To(Equal(&t.dstState))
				t.statusInfoChecker.check(clusterAfterRefresh.StatusInfo)
				if t.validationsChecker != nil {
					t.validationsChecker.check(clusterAfterRefresh.ValidationsInfo)
					Expect(clusterAfterRefresh.ValidationsInfo).ToNot(BeEmpty())
				} else {
					Expect(clusterAfterRefresh.ValidationsInfo).To(BeEmpty())
				}
			})
		}
	})
	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
		ctrl.Finish()
	})
})

var _ = Describe("Refresh Cluster - Installing Cases", func() {
	var (
		ctx                                     = context.Background()
		db                                      *gorm.DB
		clusterId, hid1, hid2, hid3, hid4, hid5 strfmt.UUID
		cluster                                 common.Cluster
		clusterApi                              *Manager
		mockEvents                              *events.MockHandler
		mockHostAPI                             *host.MockAPI
		mockMetric                              *metrics.MockAPI
		mockS3Api                               *s3wrapper.MockAPI
		mockAccountsMgmt                        *ocm.MockOCMAccountsMgmt
		operatorsManager                        *operators.Manager
		ctrl                                    *gomock.Controller
		dbName                                  string
	)

	mockHostAPIIsRequireUserActionResetFalse := func() {
		mockHostAPI.EXPECT().IsRequireUserActionReset(gomock.Any()).Return(false).AnyTimes()
	}
	mockMetricsAPIInstallationFinished := func() {
		mockMetric.EXPECT().ClusterInstallationFinished(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(1)
	}

	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		mockHostAPI = host.NewMockAPI(ctrl)
		mockMetric = metrics.NewMockAPI(ctrl)
		mockS3Api = s3wrapper.NewMockAPI(ctrl)
		operatorsManager = operators.NewManager(common.GetTestLog(), nil, operators.Options{}, nil, nil)
		clusterApi = NewManager(getDefaultConfig(), common.GetTestLog().WithField("pkg", "cluster-monitor"), db,
			mockEvents, mockHostAPI, mockMetric, nil, nil, operatorsManager, nil, mockS3Api, nil)

		hid1 = strfmt.UUID(uuid.New().String())
		hid2 = strfmt.UUID(uuid.New().String())
		hid3 = strfmt.UUID(uuid.New().String())
		hid4 = strfmt.UUID(uuid.New().String())
		hid5 = strfmt.UUID(uuid.New().String())
		clusterId = strfmt.UUID(uuid.New().String())
	})
	Context("All transitions", func() {
		var srcState string
		tests := []struct {
			name                string
			srcState            string
			srcStatusInfo       string
			machineNetworkCidr  string
			apiVip              string
			ingressVip          string
			dnsDomain           string
			pullSecretSet       bool
			dstState            string
			hosts               []models.Host
			statusInfoChecker   statusInfoChecker
			withOCMClient       bool
			requiresAMSUpdate   bool
			installationTimeout bool
			operators           []*models.MonitoredOperator
		}{
			{
				name:               "installing to installing",
				srcState:           models.ClusterStatusInstalling,
				srcStatusInfo:      statusInfoInstalling,
				dstState:           models.ClusterStatusInstalling,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.ClusterStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.ClusterStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.ClusterStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.ClusterStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.ClusterStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoInstalling),
			},
			{
				name:               "installing to installing-pending-user-action",
				srcState:           models.ClusterStatusInstalling,
				srcStatusInfo:      statusInfoInstalling,
				dstState:           models.ClusterStatusInstallingPendingUserAction,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstallingPendingUserAction), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoInstallingPendingUserAction),
			},
			{
				name:               "installing to installing-pending-user-action (2)",
				srcState:           models.ClusterStatusInstalling,
				srcStatusInfo:      statusInfoInstalling,
				dstState:           models.ClusterStatusInstallingPendingUserAction,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstallingPendingUserAction), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstallingPendingUserAction), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoInstallingPendingUserAction),
			},
			{
				name:               "installing-pending-user-action to installing-pending-user-action",
				srcState:           models.ClusterStatusInstallingPendingUserAction,
				srcStatusInfo:      statusInfoInstallingPendingUserAction,
				dstState:           models.ClusterStatusInstallingPendingUserAction,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstallingPendingUserAction), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoInstallingPendingUserAction),
			},
			{
				name:               "installing-pending-user-action to installing-pending-user-action (2)",
				srcState:           models.ClusterStatusInstallingPendingUserAction,
				srcStatusInfo:      statusInfoInstallingPendingUserAction,
				dstState:           models.ClusterStatusInstallingPendingUserAction,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstallingPendingUserAction), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoInstallingPendingUserAction),
			},
			{
				name:               "installing-pending-user-action to error",
				srcState:           models.ClusterStatusInstallingPendingUserAction,
				srcStatusInfo:      statusInfoInstallingPendingUserAction,
				dstState:           models.ClusterStatusError,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusError), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstallingPendingUserAction), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstallingPendingUserAction), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstallingPendingUserAction), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstallingPendingUserAction), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoError),
			},
			{
				name:               "installing-pending-user-action to error (2)",
				srcState:           models.ClusterStatusInstallingPendingUserAction,
				srcStatusInfo:      statusInfoInstallingPendingUserAction,
				dstState:           models.ClusterStatusError,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusError), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoError),
			},
			{
				name:               "installing-pending-user-action to error due to timeout",
				srcState:           models.ClusterStatusInstallingPendingUserAction,
				srcStatusInfo:      statusInfoInstallingPendingUserAction,
				dstState:           models.ClusterStatusError,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstallingPendingUserAction), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker:   makeValueChecker(statusInfoTimeout),
				installationTimeout: true,
			},
			{
				name:               "installing-pending-user-action to installing",
				srcState:           models.ClusterStatusInstallingPendingUserAction,
				srcStatusInfo:      statusInfoInstallingPendingUserAction,
				dstState:           models.ClusterStatusInstalling,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoInstalling),
			},
			{
				name:               "installing to finalizing",
				srcState:           models.ClusterStatusInstalling,
				srcStatusInfo:      statusInfoInstalling,
				dstState:           models.ClusterStatusFinalizing,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoFinalizing),
			},
			{
				name:               "installing to error - failing master",
				srcState:           models.ClusterStatusInstalling,
				srcStatusInfo:      statusInfoInstalling,
				dstState:           models.ClusterStatusError,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusError), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoError),
			},
			{
				name:               "installing to error - failing worker, a single worker isn't supported",
				srcState:           models.ClusterStatusInstalling,
				srcStatusInfo:      statusInfoInstalling,
				dstState:           models.ClusterStatusError,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusError), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstalling), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoError),
			},
			{
				name:               "finalizing to finalizing",
				srcState:           models.ClusterStatusFinalizing,
				srcStatusInfo:      statusInfoFinalizing,
				dstState:           models.ClusterStatusFinalizing,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoFinalizing),
				withOCMClient:     true,
				requiresAMSUpdate: true,
				operators: []*models.MonitoredOperator{
					{
						Name:         operators.OperatorConsole.Name,
						OperatorType: models.OperatorTypeBuiltin,
						Status:       models.OperatorStatusAvailable,
					},
				},
			},
			{
				name:               "finalizing to finalizing (2)",
				srcState:           models.ClusterStatusFinalizing,
				srcStatusInfo:      statusInfoFinalizing,
				dstState:           models.ClusterStatusFinalizing,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusInstalled), Inventory: common.GenerateTestDefaultInventory(), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(statusInfoFinalizing),
				withOCMClient:     true,
				// console URL should be updated only once in AMS
				requiresAMSUpdate: false,
				operators: []*models.MonitoredOperator{
					{
						Name:         operators.OperatorConsole.Name,
						OperatorType: models.OperatorTypeBuiltin,
						Status:       models.OperatorStatusAvailable,
					},
				},
			},
		}

		for i := range tests {
			t := tests[i]
			It(t.name, func() {
				cluster = common.Cluster{
					Cluster: models.Cluster{
						APIVip:                   t.apiVip,
						ID:                       &clusterId,
						IngressVip:               t.ingressVip,
						MachineNetworkCidr:       t.machineNetworkCidr,
						Status:                   &t.srcState,
						StatusInfo:               &t.srcStatusInfo,
						VipDhcpAllocation:        swag.Bool(true),
						BaseDNSDomain:            t.dnsDomain,
						PullSecretSet:            t.pullSecretSet,
						ClusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
						ClusterNetworkHostPrefix: common.TestIPv4Networking.ClusterNetworkHostPrefix,
						ServiceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
						MonitoredOperators:       t.operators,
					},
				}
				if t.withOCMClient {
					mockAccountsMgmt = ocm.NewMockOCMAccountsMgmt(ctrl)
					ocmClient := &ocm.Client{AccountsMgmt: mockAccountsMgmt, Config: &ocm.Config{}}
					clusterApi = NewManager(getDefaultConfig(), common.GetTestLog().WithField("pkg", "cluster-monitor"), db,
						mockEvents, mockHostAPI, mockMetric, nil, nil, operatorsManager, ocmClient, mockS3Api, nil)
					if !t.requiresAMSUpdate {
						cluster.IsAmsSubscriptionConsoleUrlSet = true
					}
				}
				cluster.MachineNetworkCidrUpdatedAt = time.Now().Add(-3 * time.Minute)
				if t.installationTimeout {
					// adjust the cluster InstallStartedAt to trigger a timeout
					cluster.InstallStartedAt = strfmt.DateTime(time.Now().Add(-25 * time.Hour))
				} else {
					cluster.InstallStartedAt = strfmt.DateTime(time.Now().Add(-time.Hour))
				}
				Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
				for i := range t.hosts {
					t.hosts[i].InfraEnvID = clusterId
					t.hosts[i].ClusterID = &clusterId
					Expect(db.Create(&t.hosts[i]).Error).ShouldNot(HaveOccurred())
				}
				cluster = getClusterFromDB(clusterId, db)
				if srcState != t.dstState {
					mockEvents.EXPECT().AddEvent(gomock.Any(), gomock.Any(), gomock.Any(),
						gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				}
				if t.srcState == models.ClusterStatusFinalizing && !t.requiresAMSUpdate {
					mockS3Api.EXPECT().DoesObjectExist(ctx, fmt.Sprintf("%s/%s", cluster.ID, constants.Kubeconfig)).Return(false, nil)
				}
				reportInstallationCompleteStatuses := []string{models.ClusterStatusInstalled, models.ClusterStatusError, models.ClusterStatusInstallingPendingUserAction}
				if funk.Contains(reportInstallationCompleteStatuses, t.dstState) && t.srcState != t.dstState && t.srcState != models.ClusterStatusInstallingPendingUserAction {
					mockMetricsAPIInstallationFinished()
				} else if t.dstState == models.ClusterStatusInsufficient {
					mockHostAPIIsRequireUserActionResetFalse()
				}
				if t.requiresAMSUpdate {
					subscriptionID := cluster.AmsSubscriptionID
					consoleUrl := common.GetConsoleUrl(cluster.Name, cluster.BaseDNSDomain)
					mockAccountsMgmt.EXPECT().UpdateSubscriptionConsoleUrl(ctx, subscriptionID, consoleUrl)
				}
				Expect(cluster.ValidationsInfo).To(BeEmpty())
				clusterAfterRefresh, err := clusterApi.RefreshStatus(ctx, &cluster, db)
				Expect(clusterAfterRefresh).ToNot(BeNil())
				Expect(clusterAfterRefresh.ValidationsInfo).To(BeEmpty())
				Expect(err).ToNot(HaveOccurred())
				Expect(clusterAfterRefresh.Status).To(Equal(&t.dstState))
				t.statusInfoChecker.check(clusterAfterRefresh.StatusInfo)
			})
		}
	})
	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
		ctrl.Finish()
	})
})

var _ = Describe("Log Collection - refresh cluster", func() {
	var (
		ctx         = context.Background()
		db          *gorm.DB
		clusterId   strfmt.UUID
		cluster     common.Cluster
		clusterApi  *Manager
		mockEvents  *events.MockHandler
		mockHostAPI *host.MockAPI
		mockMetric  *metrics.MockAPI
		ctrl        *gomock.Controller
		dbName      string
	)

	var (
		StatusUpdatedAt           strfmt.DateTime
		ControllerLogsCollectedAt strfmt.DateTime
		ControllerLogsStartedAt   strfmt.DateTime
		srclogsInfo               models.LogsState
		dstlogsInfo               models.LogsState
		srcState                  string
		srcStatusInfo             string
	)

	logTimeoutConfig := func() Config {
		cfg := getDefaultConfig()
		cfg.PrepareConfig.LogCollectionTimeout = 1 * time.Second
		cfg.PrepareConfig.LogPendingTimeout = 1 * time.Second
		return cfg
	}

	verifyStatusNotChanged := func(c *common.Cluster, srcState string, srcStatusInfo string) {
		Expect(c.Status).To(Equal(&srcState))
		Expect(c.StatusInfo).To(Equal(&srcStatusInfo))
		Expect(c.ValidationsInfo).To(BeEmpty())
	}

	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		mockHostAPI = host.NewMockAPI(ctrl)
		mockMetric = metrics.NewMockAPI(ctrl)
		operatorsManager := operators.NewManager(common.GetTestLog(), nil, operators.Options{}, nil, nil)
		clusterApi = NewManager(logTimeoutConfig(), common.GetTestLog().WithField("pkg", "cluster-monitor"), db,
			mockEvents, mockHostAPI, mockMetric, nil, nil, operatorsManager, nil, nil, nil)
		clusterId = strfmt.UUID(uuid.New().String())
	})

	Context("refresh on error state", func() {

		BeforeEach(func() {
			srcState = models.ClusterStatusError
			srcStatusInfo = statusInfoError
		})

		It("logs not requested when cluster enter error -> mark as timeout to signal that we do not wait for them", func() {
			srclogsInfo = ""
			dstlogsInfo = "timeout"
			StatusUpdatedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
			ControllerLogsCollectedAt = strfmt.DateTime(time.Time{})
			ControllerLogsStartedAt = strfmt.DateTime(time.Time{})
		})

		It("logs requested when cluster enter error -> timeout", func() {
			srclogsInfo = models.LogsStateRequested
			dstlogsInfo = models.LogsStateTimeout
			StatusUpdatedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
			ControllerLogsCollectedAt = strfmt.DateTime(time.Time{})
			ControllerLogsStartedAt = strfmt.DateTime(time.Time{})
		})

		It("logs requested when cluster enter error -> no timeout", func() {
			srclogsInfo = models.LogsStateRequested
			dstlogsInfo = models.LogsStateRequested
			StatusUpdatedAt = strfmt.DateTime(time.Now())
			ControllerLogsCollectedAt = strfmt.DateTime(time.Time{})
			ControllerLogsStartedAt = strfmt.DateTime(time.Now())
		})

		It("logs collected in the past but not completed -> timeout", func() {
			srclogsInfo = models.LogsStateCollecting
			dstlogsInfo = models.LogsStateTimeout
			StatusUpdatedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
			ControllerLogsCollectedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
			ControllerLogsStartedAt = strfmt.DateTime(time.Now().Add(-3 * time.Second))
		})

		It("logs collected in the past and then re-requested but not collected again -> timeout", func() {
			srclogsInfo = models.LogsStateRequested
			dstlogsInfo = models.LogsStateTimeout
			StatusUpdatedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
			ControllerLogsCollectedAt = strfmt.DateTime(time.Now().Add(-3 * time.Second))
			ControllerLogsStartedAt = strfmt.DateTime(time.Now())
		})

		It("logs collected in the past and then re-requested within timeout limits -> no timeout", func() {
			srclogsInfo = models.LogsStateRequested
			dstlogsInfo = models.LogsStateRequested
			StatusUpdatedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
			ControllerLogsCollectedAt = strfmt.DateTime(time.Now().Add(-500 * time.Millisecond))
			ControllerLogsStartedAt = strfmt.DateTime(time.Now())
		})

		It("logs completed -> no timeout", func() {
			srclogsInfo = models.LogsStateCompleted
			dstlogsInfo = models.LogsStateCompleted
			StatusUpdatedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
			ControllerLogsCollectedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
			ControllerLogsStartedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
		})
	})

	Context("refresh on cancel state", func() {

		BeforeEach(func() {
			srcState = models.ClusterStatusCancelled
			srcStatusInfo = "cancelled"
		})

		It("logs not requested when cluster enter cancel -> mark as timeout to signal that we do not wait for them", func() {
			srclogsInfo = ""
			dstlogsInfo = models.LogsStateTimeout
			StatusUpdatedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
			ControllerLogsCollectedAt = strfmt.DateTime(time.Time{})
			ControllerLogsStartedAt = strfmt.DateTime(time.Time{})
		})

		It("logs requested when cluster enter cancel -> timeout", func() {
			srclogsInfo = models.LogsStateRequested
			dstlogsInfo = models.LogsStateTimeout
			StatusUpdatedAt = strfmt.DateTime(time.Now().Add(-2 * time.Second))
			ControllerLogsCollectedAt = strfmt.DateTime(time.Time{})
			ControllerLogsStartedAt = strfmt.DateTime(time.Time{})
		})
	})
	AfterEach(func() {
		cluster = common.Cluster{
			Cluster: models.Cluster{
				ID:                        &clusterId,
				Status:                    &srcState,
				StatusInfo:                &srcStatusInfo,
				StatusUpdatedAt:           StatusUpdatedAt,
				LogsInfo:                  srclogsInfo,
				ControllerLogsCollectedAt: ControllerLogsCollectedAt,
				ControllerLogsStartedAt:   ControllerLogsStartedAt,
			},
		}
		Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
		cluster = getClusterFromDB(clusterId, db)
		clusterAfterRefresh, err := clusterApi.RefreshStatus(ctx, &cluster, db)
		Expect(err).ToNot(HaveOccurred())
		verifyStatusNotChanged(clusterAfterRefresh, srcState, srcStatusInfo)
		Expect(clusterAfterRefresh.LogsInfo).To(Equal(dstlogsInfo))
	})

	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
		ctrl.Finish()
	})
})

var _ = Describe("NTP refresh cluster", func() {
	var (
		ctx                                     = context.Background()
		db                                      *gorm.DB
		clusterId, hid1, hid2, hid3, hid4, hid5 strfmt.UUID
		cluster                                 common.Cluster
		clusterApi                              *Manager
		mockEvents                              *events.MockHandler
		mockHostAPI                             *host.MockAPI
		mockMetric                              *metrics.MockAPI
		ctrl                                    *gomock.Controller
		dbName                                  string
	)

	mockHostAPIIsRequireUserActionResetFalse := func() {
		mockHostAPI.EXPECT().IsRequireUserActionReset(gomock.Any()).Return(false).AnyTimes()
	}
	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		mockHostAPI = host.NewMockAPI(ctrl)
		mockMetric = metrics.NewMockAPI(ctrl)
		operatorsManager := operators.NewManager(common.GetTestLog(), nil, operators.Options{}, nil, nil)
		clusterApi = NewManager(getDefaultConfig(), common.GetTestLog().WithField("pkg", "cluster-monitor"), db,
			mockEvents, mockHostAPI, mockMetric, nil, nil, operatorsManager, nil, nil, nil)

		hid1 = strfmt.UUID(uuid.New().String())
		hid2 = strfmt.UUID(uuid.New().String())
		hid3 = strfmt.UUID(uuid.New().String())
		hid4 = strfmt.UUID(uuid.New().String())
		hid5 = strfmt.UUID(uuid.New().String())
		clusterId = strfmt.UUID(uuid.New().String())
	})
	Context("All transitions", func() {
		var srcState string
		tests := []struct {
			name                    string
			srcState                string
			srcStatusInfo           string
			machineNetworkCidr      string
			apiVip                  string
			ingressVip              string
			dnsDomain               string
			pullSecretSet           bool
			dstState                string
			hosts                   []models.Host
			statusInfoChecker       statusInfoChecker
			validationsChecker      *validationsChecker
			setMachineCidrUpdatedAt bool
			errorExpected           bool
		}{
			{
				name:               "pending-for-input to insufficient - ntp problem",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239 - 400), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates"},
					IsNtpServerConfigured:               {status: ValidationFailure, messagePattern: "please configure an NTP server via DHCP"},
				}),
				errorExpected: false,
			},
			{
				name:               "pending-for-input to ready",
				srcState:           models.ClusterStatusPendingForInput,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates"},
					IsNtpServerConfigured:               {status: ValidationSuccess, messagePattern: "No ntp problems found"},
				}),
				errorExpected: false,
			},
			{
				name:               "insufficient to ready",
				srcState:           models.ClusterStatusInsufficient,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates"},
					IsNtpServerConfigured:               {status: ValidationSuccess, messagePattern: "No ntp problems found"},
				}),
				errorExpected: false,
			},
			{
				name:               "ready to ready",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates"},
					IsNtpServerConfigured:               {status: ValidationSuccess, messagePattern: "No ntp problems found"},
				}),
				errorExpected: false,
			},

			{
				name:               "ready to ready with disabled",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusDisabled), Inventory: defaultInventoryWithTimestamp(1601909239 + 1000), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusDisabled), Inventory: defaultInventoryWithTimestamp(1601909239 - 1000), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates"},
					IsNtpServerConfigured:               {status: ValidationSuccess, messagePattern: "No ntp problems found"},
				}),
				errorExpected: false,
			},

			{
				name:               "ready to insufficient with disconnected",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusDisconnected), Inventory: defaultInventoryWithTimestamp(1601909239 + 1000), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusDisconnected), Inventory: defaultInventoryWithTimestamp(1601909239 - 1000), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationFailure, messagePattern: "The cluster has hosts that are not ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates"},
					IsNtpServerConfigured:               {status: ValidationSuccess, messagePattern: "No ntp problems found"},
				}),
				errorExpected: false,
			},

			{
				name:               "ready to insufficient with needs o be rebooted status",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid4, Status: swag.String(models.HostStatusResettingPendingUserAction), Inventory: defaultInventoryWithTimestamp(1601909239 + 1000), Role: models.HostRoleWorker},
					{ID: &hid5, Status: swag.String(models.HostStatusResettingPendingUserAction), Inventory: defaultInventoryWithTimestamp(1601909239 - 1000), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined."},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR."},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined."},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined."},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationFailure, messagePattern: "The cluster has hosts that are not ready to install."},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined."},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set."},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
					IsNtpServerConfigured:               {status: ValidationSuccess, messagePattern: "No ntp problems found"},
				}),
				errorExpected: false,
			},

			{
				name:               "ready to insufficient",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239 - 400), Role: models.HostRoleMaster},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates"},
					IsNtpServerConfigured:               {status: ValidationFailure, messagePattern: "please configure an NTP server via DHCP"},
				}),
				errorExpected: false,
			},
		}
		for i := range tests {
			t := tests[i]
			It(t.name, func() {
				cluster = common.Cluster{
					Cluster: models.Cluster{
						APIVip:                   t.apiVip,
						ID:                       &clusterId,
						IngressVip:               t.ingressVip,
						MachineNetworkCidr:       t.machineNetworkCidr,
						Status:                   &t.srcState,
						StatusInfo:               &t.srcStatusInfo,
						BaseDNSDomain:            t.dnsDomain,
						PullSecretSet:            t.pullSecretSet,
						ClusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
						ClusterNetworkHostPrefix: common.TestIPv4Networking.ClusterNetworkHostPrefix,
						ServiceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
						NetworkType:              swag.String(models.ClusterNetworkTypeOVNKubernetes),
					},
				}
				Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
				for i := range t.hosts {
					t.hosts[i].InfraEnvID = clusterId
					t.hosts[i].ClusterID = &clusterId
					Expect(db.Create(&t.hosts[i]).Error).ShouldNot(HaveOccurred())
				}
				cluster = getClusterFromDB(clusterId, db)
				if srcState != t.dstState {
					mockEvents.EXPECT().AddEvent(gomock.Any(), gomock.Any(), gomock.Any(),
						gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
				}
				mockHostAPIIsRequireUserActionResetFalse()
				if t.dstState == models.ClusterStatusInsufficient {
					mockHostAPIIsRequireUserActionResetFalse()
				}
				Expect(cluster.ValidationsInfo).To(BeEmpty())
				clusterAfterRefresh, err := clusterApi.RefreshStatus(ctx, &cluster, db)
				if t.errorExpected {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).ToNot(HaveOccurred())
				}
				Expect(clusterAfterRefresh.Status).To(Equal(&t.dstState))
				t.statusInfoChecker.check(clusterAfterRefresh.StatusInfo)
				if t.validationsChecker != nil {
					t.validationsChecker.check(clusterAfterRefresh.ValidationsInfo)
					Expect(clusterAfterRefresh.ValidationsInfo).ToNot(BeEmpty())
				} else {
					Expect(clusterAfterRefresh.ValidationsInfo).To(BeEmpty())
				}
			})
		}
	})
	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
		ctrl.Finish()
	})
})

var _ = Describe("Single node", func() {
	var (
		ctx                         = context.Background()
		db                          *gorm.DB
		clusterId, hid1, hid2, hid3 strfmt.UUID
		cluster                     common.Cluster
		clusterApi                  *Manager
		mockEvents                  *events.MockHandler
		mockHostAPI                 *host.MockAPI
		mockMetric                  *metrics.MockAPI
		ctrl                        *gomock.Controller
		dbName                      string
	)

	mockHostAPIIsRequireUserActionResetFalse := func() {
		mockHostAPI.EXPECT().IsRequireUserActionReset(gomock.Any()).Return(false).AnyTimes()
	}
	mockIsValidMasterCandidate := func() {
		mockHostAPI.EXPECT().IsValidMasterCandidate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil).AnyTimes()
	}
	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		mockHostAPI = host.NewMockAPI(ctrl)
		mockMetric = metrics.NewMockAPI(ctrl)
		operatorsManager := operators.NewManager(common.GetTestLog(), nil, operators.Options{}, nil, nil)
		dnsApi := dns.NewDNSHandler(nil, common.GetTestLog())
		clusterApi = NewManager(getDefaultConfig(), common.GetTestLog().WithField("pkg", "cluster-monitor"), db,
			mockEvents, mockHostAPI, mockMetric, nil, nil, operatorsManager, nil, nil, dnsApi)
		hid1 = strfmt.UUID(uuid.New().String())
		hid2 = strfmt.UUID(uuid.New().String())
		hid3 = strfmt.UUID(uuid.New().String())
		clusterId = strfmt.UUID(uuid.New().String())
	})
	Context("single node transitions", func() {
		var srcState string
		tests := []struct {
			name                    string
			srcState                string
			srcStatusInfo           string
			machineNetworkCidr      string
			apiVip                  string
			ingressVip              string
			dnsDomain               string
			pullSecretSet           bool
			dstState                string
			hosts                   []models.Host
			statusInfoChecker       statusInfoChecker
			validationsChecker      *validationsChecker
			setMachineCidrUpdatedAt bool
			errorExpected           bool
		}{
			{
				name:               "non ha mode, too much nodes",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationFailure, messagePattern: "Single-node clusters must have a single master node and no workers."},
				}),
				errorExpected: false,
			},
			{
				name:               "non ha mode, 2 nodes, one disabled",
				srcState:           models.ClusterStatusInsufficient,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusDisabled), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},

			{
				name:               "non ha mode, 2 nodes, master and worker",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationFailure, messagePattern: "Single-node clusters must have a single master node and no workers."},
				}),
				errorExpected: false,
			},

			{
				name:               "non ha mode, 3 nodes, master and 2 workers",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleWorker},
					{ID: &hid3, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationFailure, messagePattern: "Single-node clusters must have a single master node and no workers."},
				}),
				errorExpected: false,
			},

			{
				name:               "non ha mode, 1 worker, no masters",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusInsufficient,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleWorker},
				},
				statusInfoChecker: makeValueChecker(StatusInfoInsufficient),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationFailure, messagePattern: "Single-node clusters must have a single master node and no workers."},
				}),
				errorExpected: false,
			},

			{
				name:               "non ha mode, 1 node master",
				srcState:           models.ClusterStatusReady,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},

			{
				name:               "non ha mode, 1 node auto assign",
				srcState:           models.ClusterStatusInsufficient,
				dstState:           models.ClusterStatusReady,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid2, Status: swag.String(models.HostStatusKnown), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleAutoAssign},
				},
				statusInfoChecker: makeValueChecker(StatusInfoReady),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},
			{
				name:               "set API and Ingress vip for SNO",
				srcState:           models.ClusterStatusPreparingForInstallation,
				srcStatusInfo:      statusInfoInstalling,
				dstState:           models.ClusterStatusInstalling,
				machineNetworkCidr: common.TestIPv4Networking.MachineNetworkCidr,
				apiVip:             common.TestIPv4Networking.APIVip,
				ingressVip:         common.TestIPv4Networking.IngressVip,
				dnsDomain:          "test.com",
				pullSecretSet:      true,
				hosts: []models.Host{
					{ID: &hid1, Status: swag.String(models.HostStatusPreparingSuccessful), Inventory: defaultInventoryWithTimestamp(1601909239), Role: models.HostRoleMaster, Bootstrap: true},
				},
				statusInfoChecker: makeValueChecker(statusInfoInstalling),
				validationsChecker: makeJsonChecker(map[ValidationID]validationCheckResult{
					IsMachineCidrDefined:                {status: ValidationSuccess, messagePattern: "The Machine Network CIDR is defined"},
					IsMachineCidrEqualsToCalculatedCidr: {status: ValidationSuccess, messagePattern: "The Cluster Machine CIDR is equivalent to the calculated CIDR"},
					IsApiVipDefined:                     {status: ValidationSuccess, messagePattern: "The API virtual IP is defined"},
					IsApiVipValid:                       {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					IsIngressVipDefined:                 {status: ValidationSuccess, messagePattern: "The Ingress virtual IP is defined"},
					IsIngressVipValid:                   {status: ValidationSuccess, messagePattern: "belongs to the Machine CIDR and is not in use."},
					AllHostsAreReadyToInstall:           {status: ValidationSuccess, messagePattern: "All hosts in the cluster are ready to install"},
					IsDNSDomainDefined:                  {status: ValidationSuccess, messagePattern: "The base domain is defined"},
					IsPullSecretSet:                     {status: ValidationSuccess, messagePattern: "The pull secret is set"},
					SufficientMastersCount:              {status: ValidationSuccess, messagePattern: "The cluster has a sufficient number of master candidates."},
				}),
				errorExpected: false,
			},
		}
		for i := range tests {
			t := tests[i]
			haMode := models.ClusterHighAvailabilityModeNone
			It(t.name, func() {
				cluster = common.Cluster{
					Cluster: models.Cluster{
						APIVip:                   t.apiVip,
						ID:                       &clusterId,
						IngressVip:               t.ingressVip,
						MachineNetworkCidr:       t.machineNetworkCidr,
						Status:                   &t.srcState,
						StatusInfo:               &t.srcStatusInfo,
						BaseDNSDomain:            t.dnsDomain,
						PullSecretSet:            t.pullSecretSet,
						ClusterNetworkCidr:       common.TestIPv4Networking.ClusterNetworkCidr,
						ClusterNetworkHostPrefix: common.TestIPv4Networking.ClusterNetworkHostPrefix,
						ServiceNetworkCidr:       common.TestIPv4Networking.ServiceNetworkCidr,
						NetworkType:              swag.String(models.ClusterNetworkTypeOVNKubernetes),
						HighAvailabilityMode:     &haMode,
					},
				}
				if t.srcState == models.ClusterStatusPreparingForInstallation && t.dstState == models.ClusterStatusInstalling {
					cluster.Cluster.StatusUpdatedAt = strfmt.DateTime(time.Now())
					cluster.InstallationPreparationCompletionStatus = common.InstallationPreparationSucceeded

					mockMetric.EXPECT().InstallationStarted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(1)
					mockMetric.EXPECT().ClusterHostInstallationCount(gomock.Any(), gomock.Any(), gomock.Any()).Times(1)

				}
				Expect(db.Create(&cluster).Error).ShouldNot(HaveOccurred())
				mockIsValidMasterCandidate()
				for i := range t.hosts {
					t.hosts[i].InfraEnvID = clusterId
					t.hosts[i].ClusterID = &clusterId
					Expect(db.Create(&t.hosts[i]).Error).ShouldNot(HaveOccurred())
				}
				cluster = getClusterFromDB(clusterId, db)
				if srcState != t.dstState {
					mockEvents.EXPECT().AddEvent(gomock.Any(), gomock.Any(), gomock.Any(),
						gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

				}
				mockHostAPIIsRequireUserActionResetFalse()

				if t.dstState == models.ClusterStatusInsufficient {
					mockHostAPIIsRequireUserActionResetFalse()
				}
				clusterAfterRefresh, err := clusterApi.RefreshStatus(ctx, &cluster, db)
				if t.errorExpected {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).ToNot(HaveOccurred())
				}
				Expect(clusterAfterRefresh.Status).To(Equal(&t.dstState))
				t.statusInfoChecker.check(clusterAfterRefresh.StatusInfo)
				if t.validationsChecker != nil {
					t.validationsChecker.check(clusterAfterRefresh.ValidationsInfo)
				}
			})
		}
	})
	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
		ctrl.Finish()
	})
})

var _ = Describe("ValidationResult sort", func() {
	It("ValidationResult sort", func() {
		validationResults := []ValidationResult{
			{ID: "cab", Status: "abc", Message: "abc"},
			{ID: "bac", Status: "abc", Message: "abc"},
			{ID: "acb", Status: "abc", Message: "abc"},
			{ID: "abc", Status: "abc", Message: "abc"},
		}
		sortByValidationResultID(validationResults)
		Expect(validationResults[0].ID.String()).Should(Equal("abc"))
		Expect(validationResults[1].ID.String()).Should(Equal("acb"))
		Expect(validationResults[2].ID.String()).Should(Equal("bac"))
		Expect(validationResults[3].ID.String()).Should(Equal("cab"))
	})
})

func getClusterFromDB(clusterId strfmt.UUID, db *gorm.DB) common.Cluster {
	c, err := common.GetClusterFromDB(db, clusterId, common.UseEagerLoading)
	Expect(err).ShouldNot(HaveOccurred())
	return *c
}
