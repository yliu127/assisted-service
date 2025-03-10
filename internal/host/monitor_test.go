package host

import (
	"context"
	"fmt"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/postgres"
	"github.com/kelseyhightower/envconfig"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/openshift/assisted-service/internal/common"
	"github.com/openshift/assisted-service/internal/events"
	"github.com/openshift/assisted-service/internal/hardware"
	"github.com/openshift/assisted-service/internal/host/hostutil"
	"github.com/openshift/assisted-service/internal/metrics"
	"github.com/openshift/assisted-service/internal/operators"
	"github.com/openshift/assisted-service/internal/operators/api"
	"github.com/openshift/assisted-service/models"
	"github.com/openshift/assisted-service/pkg/leader"
)

var _ = Describe("monitor_disconnection", func() {
	var (
		ctx           = context.Background()
		db            *gorm.DB
		state         API
		host          models.Host
		ctrl          *gomock.Controller
		mockEvents    *events.MockHandler
		dbName        string
		mockMetricApi *metrics.MockAPI
	)

	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		dummy := &leader.DummyElector{}
		mockHwValidator := hardware.NewMockValidator(ctrl)
		mockMetricApi = metrics.NewMockAPI(ctrl)
		mockHwValidator.EXPECT().ListEligibleDisks(gomock.Any()).AnyTimes()
		mockHwValidator.EXPECT().GetClusterHostRequirements(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().Return(&models.ClusterHostRequirements{
			Total: &models.ClusterHostRequirementsDetails{},
		}, nil)
		mockHwValidator.EXPECT().GetPreflightHardwareRequirements(gomock.Any(), gomock.Any()).AnyTimes().Return(&models.PreflightHardwareRequirements{
			Ocp: &models.HostTypeHardwareRequirementsWrapper{
				Worker: &models.HostTypeHardwareRequirements{
					Quantitative: &models.ClusterHostRequirementsDetails{},
				},
				Master: &models.HostTypeHardwareRequirements{
					Quantitative: &models.ClusterHostRequirementsDetails{},
				},
			},
		}, nil)
		mockHwValidator.EXPECT().GetHostValidDisks(gomock.Any()).Return(nil, nil).AnyTimes()
		mockOperators := operators.NewMockAPI(ctrl)
		state = NewManager(common.GetTestLog(), db, mockEvents, mockHwValidator, nil, createValidatorCfg(),
			mockMetricApi, defaultConfig, dummy, mockOperators)
		clusterID := strfmt.UUID(uuid.New().String())
		infraEnvID := strfmt.UUID(uuid.New().String())
		host = hostutil.GenerateTestHost(strfmt.UUID(uuid.New().String()), infraEnvID, clusterID, models.HostStatusDiscovering)
		cluster := hostutil.GenerateTestCluster(clusterID, "1.1.0.0/16")
		Expect(db.Save(&cluster).Error).ToNot(HaveOccurred())
		host.Inventory = workerInventory()
		err := state.RegisterHost(ctx, &host, db)
		Expect(err).ShouldNot(HaveOccurred())
		db.First(&host, "id = ? and cluster_id = ?", host.ID, host.ClusterID)

		mockMetricApi.EXPECT().Duration("HostMonitoring", gomock.Any()).Times(1)
		mockMetricApi.EXPECT().MonitoredHostsCount(gomock.Any()).Times(1)
		mockOperators.EXPECT().ValidateHost(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().Return([]api.ValidationResult{
			{Status: api.Success, ValidationId: string(models.HostValidationIDOcsRequirementsSatisfied)},
			{Status: api.Success, ValidationId: string(models.HostValidationIDLsoRequirementsSatisfied)},
			{Status: api.Success, ValidationId: string(models.HostValidationIDCnvRequirementsSatisfied)},
		}, nil)
		mockHwValidator.EXPECT().GetHostInstallationPath(gomock.Any()).Return("abc").AnyTimes()
	})

	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
	})

	Context("host_disconnecting", func() {
		It("known_host_disconnects", func() {
			host.CheckedInAt = strfmt.DateTime(time.Now().Add(-4 * time.Minute))
			host.Status = swag.String(models.HostStatusKnown)
			db.Save(&host)
		})

		It("discovering_host_disconnects", func() {
			host.CheckedInAt = strfmt.DateTime(time.Now().Add(-4 * time.Minute))
			host.Status = swag.String(models.HostStatusDiscovering)
			db.Save(&host)
		})

		It("known_host_insufficient", func() {
			host.CheckedInAt = strfmt.DateTime(time.Now().Add(-4 * time.Minute))
			host.Status = swag.String(models.HostStatusInsufficient)
			db.Save(&host)
		})

		AfterEach(func() {
			mockEvents.EXPECT().AddEvent(gomock.Any(), host.InfraEnvID, host.ID, models.EventSeverityWarning,
				fmt.Sprintf("Host %s: updated status from \"%s\" to \"disconnected\" (Host has stopped communicating with the installation service)",
					host.ID.String(), *host.Status),
				gomock.Any())
			state.HostMonitoring()
			db.First(&host, "id = ? and cluster_id = ?", host.ID, host.ClusterID)
			Expect(*host.Status).Should(Equal(models.HostStatusDisconnected))
		})
	})

	Context("host_reconnecting", func() {
		It("host_connects", func() {
			host.CheckedInAt = strfmt.DateTime(time.Now())
			host.Inventory = ""
			host.Status = swag.String(models.HostStatusDisconnected)
			db.Save(&host)
		})

		AfterEach(func() {
			mockEvents.EXPECT().AddEvent(gomock.Any(), host.InfraEnvID, host.ID, models.EventSeverityInfo,
				fmt.Sprintf("Host %s: updated status from \"disconnected\" to \"discovering\" (Waiting for host to send hardware details)", host.ID.String()),
				gomock.Any())
			state.HostMonitoring()
			db.First(&host, "id = ? and cluster_id = ?", host.ID, host.ClusterID)
			Expect(*host.Status).Should(Equal(models.HostStatusDiscovering))
		})
	})

	AfterEach(func() {
		ctrl.Finish()
		db.Close()
	})
})

var _ = Describe("TestHostMonitoring - with cluster", func() {
	var (
		ctx           = context.Background()
		db            *gorm.DB
		state         API
		host          models.Host
		ctrl          *gomock.Controller
		cfg           Config
		mockEvents    *events.MockHandler
		dbName        string
		clusterID     = strfmt.UUID(uuid.New().String())
		infraEnvID    = strfmt.UUID(uuid.New().String())
		mockMetricApi *metrics.MockAPI
	)

	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		mockEvents.EXPECT().
			AddEvent(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			AnyTimes()
		Expect(envconfig.Process(common.EnvConfigPrefix, &cfg)).ShouldNot(HaveOccurred())
		mockMetricApi = metrics.NewMockAPI(ctrl)
		mockHwValidator := hardware.NewMockValidator(ctrl)
		mockHwValidator.EXPECT().ListEligibleDisks(gomock.Any()).AnyTimes()
		mockHwValidator.EXPECT().GetClusterHostRequirements(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().Return(&models.ClusterHostRequirements{
			Total: &models.ClusterHostRequirementsDetails{},
		}, nil)
		mockHwValidator.EXPECT().GetPreflightHardwareRequirements(gomock.Any(), gomock.Any()).AnyTimes().Return(&models.PreflightHardwareRequirements{
			Ocp: &models.HostTypeHardwareRequirementsWrapper{
				Worker: &models.HostTypeHardwareRequirements{
					Quantitative: &models.ClusterHostRequirementsDetails{},
				},
				Master: &models.HostTypeHardwareRequirements{
					Quantitative: &models.ClusterHostRequirementsDetails{},
				},
			},
		}, nil)
		mockHwValidator.EXPECT().GetHostValidDisks(gomock.Any()).Return(nil, nil).AnyTimes()
		mockOperators := operators.NewMockAPI(ctrl)
		state = NewManager(common.GetTestLog(), db, mockEvents, mockHwValidator, nil, createValidatorCfg(),
			mockMetricApi, &cfg, &leader.DummyElector{}, mockOperators)

		mockMetricApi.EXPECT().Duration("HostMonitoring", gomock.Any()).Times(1)
		mockOperators.EXPECT().ValidateHost(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().Return([]api.ValidationResult{
			{Status: api.Success, ValidationId: string(models.HostValidationIDOcsRequirementsSatisfied)},
			{Status: api.Success, ValidationId: string(models.HostValidationIDLsoRequirementsSatisfied)},
			{Status: api.Success, ValidationId: string(models.HostValidationIDCnvRequirementsSatisfied)},
		}, nil)
		mockHwValidator.EXPECT().GetHostInstallationPath(gomock.Any()).Return("abc").AnyTimes()
	})

	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
		ctrl.Finish()
	})

	Context("validate monitor in batches", func() {
		registerAndValidateDisconnected := func(nHosts int) {
			for i := 0; i < nHosts; i++ {
				if i%10 == 0 {
					clusterID = strfmt.UUID(uuid.New().String())
					cluster := hostutil.GenerateTestCluster(clusterID, "1.1.0.0/16")
					Expect(db.Save(&cluster).Error).ToNot(HaveOccurred())
				}
				host = hostutil.GenerateTestHost(strfmt.UUID(uuid.New().String()), infraEnvID, clusterID, models.HostStatusDiscovering)
				host.Inventory = workerInventory()
				Expect(state.RegisterHost(ctx, &host, db)).ShouldNot(HaveOccurred())
				host.CheckedInAt = strfmt.DateTime(time.Now().Add(-4 * time.Minute))
				db.Save(&host)
			}
			state.HostMonitoring()
			var count int
			Expect(db.Model(&models.Host{}).Where("status = ?", models.HostStatusDisconnected).Count(&count).Error).
				ShouldNot(HaveOccurred())
			Expect(count).Should(Equal(nHosts))
		}

		It("5 hosts all disconnected", func() {
			mockMetricApi.EXPECT().MonitoredHostsCount(int64(5)).Times(1)
			registerAndValidateDisconnected(5)
		})

		It("15 hosts all disconnected", func() {
			mockMetricApi.EXPECT().MonitoredHostsCount(int64(15)).Times(1)
			registerAndValidateDisconnected(15)
		})

		It("765 hosts all disconnected", func() {
			mockMetricApi.EXPECT().MonitoredHostsCount(int64(765)).Times(1)
			registerAndValidateDisconnected(765)
		})
	})
})

var _ = Describe("TestHostMonitoring - with infra-env", func() {
	var (
		ctx           = context.Background()
		db            *gorm.DB
		state         API
		host          models.Host
		ctrl          *gomock.Controller
		cfg           Config
		mockEvents    *events.MockHandler
		dbName        string
		infraEnvID    = strfmt.UUID(uuid.New().String())
		mockMetricApi *metrics.MockAPI
	)

	BeforeEach(func() {
		db, dbName = common.PrepareTestDB()
		ctrl = gomock.NewController(GinkgoT())
		mockEvents = events.NewMockHandler(ctrl)
		mockEvents.EXPECT().
			AddEvent(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			AnyTimes()
		Expect(envconfig.Process(common.EnvConfigPrefix, &cfg)).ShouldNot(HaveOccurred())
		mockMetricApi = metrics.NewMockAPI(ctrl)
		mockHwValidator := hardware.NewMockValidator(ctrl)
		mockHwValidator.EXPECT().ListEligibleDisks(gomock.Any()).AnyTimes()
		mockHwValidator.EXPECT().GetInfraEnvHostRequirements(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().Return(&models.ClusterHostRequirements{
			Total: &models.ClusterHostRequirementsDetails{},
		}, nil)
		mockHwValidator.EXPECT().GetPreflightInfraEnvHardwareRequirements(gomock.Any(), gomock.Any()).AnyTimes().Return(&models.PreflightHardwareRequirements{
			Ocp: &models.HostTypeHardwareRequirementsWrapper{
				Worker: &models.HostTypeHardwareRequirements{
					Quantitative: &models.ClusterHostRequirementsDetails{},
				},
				Master: &models.HostTypeHardwareRequirements{
					Quantitative: &models.ClusterHostRequirementsDetails{},
				},
			},
		}, nil)
		mockHwValidator.EXPECT().GetHostValidDisks(gomock.Any()).Return(nil, nil).AnyTimes()
		mockOperators := operators.NewMockAPI(ctrl)
		state = NewManager(common.GetTestLog(), db, mockEvents, mockHwValidator, nil, createValidatorCfg(),
			mockMetricApi, &cfg, &leader.DummyElector{}, mockOperators)

		mockMetricApi.EXPECT().Duration("HostMonitoring", gomock.Any()).Times(1)
		mockOperators.EXPECT().ValidateHost(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes().Return([]api.ValidationResult{
			{Status: api.Success, ValidationId: string(models.HostValidationIDOcsRequirementsSatisfied)},
			{Status: api.Success, ValidationId: string(models.HostValidationIDLsoRequirementsSatisfied)},
			{Status: api.Success, ValidationId: string(models.HostValidationIDCnvRequirementsSatisfied)},
		}, nil)
		mockHwValidator.EXPECT().GetHostInstallationPath(gomock.Any()).Return("abc").AnyTimes()
	})

	AfterEach(func() {
		common.DeleteTestDB(db, dbName)
		ctrl.Finish()
	})

	Context("validate monitor in batches", func() {
		registerAndValidateDisconnected := func(nHosts int) {
			for i := 0; i < nHosts; i++ {
				if i%10 == 0 {
					infraEnvID = strfmt.UUID(uuid.New().String())
					infraEnv := hostutil.GenerateTestInfraEnv(infraEnvID)
					Expect(db.Save(infraEnv).Error).ToNot(HaveOccurred())
				}
				host = hostutil.GenerateTestHostWithInfraEnv(strfmt.UUID(uuid.New().String()), infraEnvID, models.HostStatusDiscoveringUnbound, models.HostRoleWorker)
				host.Inventory = workerInventory()
				Expect(state.RegisterHost(ctx, &host, db)).ShouldNot(HaveOccurred())
				host.CheckedInAt = strfmt.DateTime(time.Now().Add(-4 * time.Minute))
				db.Save(&host)
			}
			state.HostMonitoring()
			var count int
			Expect(db.Model(&models.Host{}).Where("status = ?", models.HostStatusDisconnectedUnbound).Count(&count).Error).
				ShouldNot(HaveOccurred())
			Expect(count).Should(Equal(nHosts))
		}

		registerAndValidateDisconnectedAndDisabled := func(nDisconnected, nDisabled int) {
			for i := 0; i < nDisconnected; i++ {
				if i%10 == 0 {
					infraEnvID = strfmt.UUID(uuid.New().String())
					infraEnv := hostutil.GenerateTestInfraEnv(infraEnvID)
					Expect(db.Save(infraEnv).Error).ToNot(HaveOccurred())
				}
				host = hostutil.GenerateTestHostWithInfraEnv(strfmt.UUID(uuid.New().String()), infraEnvID, models.HostStatusDiscoveringUnbound, models.HostRoleWorker)
				host.Inventory = workerInventory()
				Expect(state.RegisterHost(ctx, &host, db)).ShouldNot(HaveOccurred())
				host.CheckedInAt = strfmt.DateTime(time.Now().Add(-4 * time.Minute))
				db.Save(&host)
			}
			for i := 0; i < nDisabled; i++ {
				if i%10 == 0 {
					infraEnvID = strfmt.UUID(uuid.New().String())
					infraEnv := hostutil.GenerateTestInfraEnv(infraEnvID)
					Expect(db.Save(infraEnv).Error).ToNot(HaveOccurred())
				}
				host = hostutil.GenerateTestHostWithInfraEnv(strfmt.UUID(uuid.New().String()), infraEnvID, models.HostStatusDisabledUnbound, models.HostRoleWorker)
				host.Inventory = workerInventory()
				Expect(state.RegisterHost(ctx, &host, db)).ShouldNot(HaveOccurred())
				host.CheckedInAt = strfmt.DateTime(time.Now().Add(-4 * time.Minute))
				db.Save(&host)
			}
			state.HostMonitoring()
			var count int
			Expect(db.Model(&models.Host{}).Where("status = ?", models.HostStatusDisconnectedUnbound).Count(&count).Error).
				ShouldNot(HaveOccurred())
			Expect(count).Should(Equal(nDisconnected))
		}

		It("5 hosts all disconnected", func() {
			mockMetricApi.EXPECT().MonitoredHostsCount(int64(5)).Times(1)
			registerAndValidateDisconnected(5)
		})

		It("15 hosts all disconnected", func() {
			mockMetricApi.EXPECT().MonitoredHostsCount(int64(15)).Times(1)
			registerAndValidateDisconnected(15)
		})

		It("765 hosts all disconnected", func() {
			mockMetricApi.EXPECT().MonitoredHostsCount(int64(765)).Times(1)
			registerAndValidateDisconnected(765)
		})
		It("765 hosts disconnected, 5 disabled", func() {
			mockMetricApi.EXPECT().MonitoredHostsCount(int64(765)).Times(1)
			registerAndValidateDisconnectedAndDisabled(765, 5)
		})
	})
})
