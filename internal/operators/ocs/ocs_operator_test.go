package ocs

import (
	"context"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	"github.com/openshift/assisted-service/internal/common"
	"github.com/openshift/assisted-service/internal/operators/api"
	"github.com/openshift/assisted-service/models"
	"github.com/openshift/assisted-service/pkg/conversions"
)

var _ = Describe("Ocs Operator", func() {
	var (
		ctx                 = context.TODO()
		operator            = NewOcsOperator(common.GetTestLog(), nil)
		diskID1             = "/dev/disk/by-id/test-disk-1"
		diskID2             = "/dev/disk/by-id/test-disk-2"
		diskID3             = "/dev/disk/by-id/test-disk-3"
		masterWithThreeDisk = &models.Host{Role: models.HostRoleMaster, InstallationDiskID: diskID1,
			Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 32 * conversions.GiB,
				Disks: []*models.Disk{
					{SizeBytes: 20 * conversions.GB, DriveType: "HDD", ID: diskID1},
					{SizeBytes: 40 * conversions.GB, DriveType: "SSD", ID: diskID2},
					{SizeBytes: 40 * conversions.GB, DriveType: "SSD", ID: diskID3},
				}})}
		masterWithThreeDiskSizeOfOneZero = &models.Host{Role: models.HostRoleMaster, InstallationDiskID: diskID1,
			Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 32 * conversions.GiB,
				Disks: []*models.Disk{
					{SizeBytes: 20 * conversions.GB, DriveType: "HDD", ID: diskID1},
					{SizeBytes: 40 * conversions.GB, DriveType: "SSD", ID: diskID2},
					{SizeBytes: 0 * conversions.GB, DriveType: "SSD", ID: diskID3},
				}})}
		masterWithNoDisk      = &models.Host{Role: models.HostRoleMaster, Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 32 * conversions.GiB})}
		masterWithNoInventory = &models.Host{Role: models.HostRoleMaster}
		masterWithOneDisk     = &models.Host{Role: models.HostRoleMaster, InstallationDiskID: diskID1,
			Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 32 * conversions.GiB,
				Disks: []*models.Disk{
					{SizeBytes: 20 * conversions.GB, DriveType: "HDD", ID: diskID1}}})}
		masterWithLessDiskSize = &models.Host{Role: models.HostRoleMaster, InstallationDiskID: diskID1,
			Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 32 * conversions.GiB,
				Disks: []*models.Disk{
					{SizeBytes: 20 * conversions.GB, DriveType: "HDD", ID: diskID1},
					{SizeBytes: 40 * conversions.GB, DriveType: "SSD", ID: diskID2},
					{SizeBytes: 20 * conversions.GB, DriveType: "SSD", ID: diskID2},
				}})}
		workerWithOneDisk = &models.Host{Role: models.HostRoleWorker, InstallationDiskID: diskID1,
			Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 64 * conversions.GiB,
				Disks: []*models.Disk{
					{SizeBytes: 20 * conversions.GB, DriveType: "HDD", ID: diskID1},
				}})}
		workerWithTwoDisk = &models.Host{Role: models.HostRoleWorker, InstallationDiskID: diskID1,
			Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 64 * conversions.GiB,
				Disks: []*models.Disk{
					{SizeBytes: 20 * conversions.GB, DriveType: "HDD", ID: diskID1},
					{SizeBytes: 40 * conversions.GB, DriveType: "SSD", ID: diskID2},
				}})}
		workerWithThreeDisk = &models.Host{Role: models.HostRoleWorker, InstallationDiskID: diskID1,
			Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 64 * conversions.GiB,
				Disks: []*models.Disk{
					{SizeBytes: 20 * conversions.GB, DriveType: "HDD", ID: diskID1},
					{SizeBytes: 40 * conversions.GB, DriveType: "SSD", ID: diskID2},
					{SizeBytes: 40 * conversions.GB, DriveType: "HDD", ID: diskID3},
				}})}
		workerWithThreeDiskSizeOfOneZero = &models.Host{Role: models.HostRoleWorker, InstallationDiskID: diskID1,
			Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 64 * conversions.GiB,
				Disks: []*models.Disk{
					{SizeBytes: 20 * conversions.GB, DriveType: "HDD", ID: diskID1},
					{SizeBytes: 40 * conversions.GB, DriveType: "SSD", ID: diskID2},
					{SizeBytes: 0 * conversions.GB, DriveType: "HDD", ID: diskID3},
				}})}
		workerWithNoDisk       = &models.Host{Role: models.HostRoleWorker, Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 64 * conversions.GiB})}
		workerWithNoInventory  = &models.Host{Role: models.HostRoleWorker}
		workerWithLessDiskSize = &models.Host{Role: models.HostRoleWorker, InstallationDiskID: diskID1,
			Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 32 * conversions.GiB,
				Disks: []*models.Disk{
					{SizeBytes: 20 * conversions.GB, DriveType: "HDD", ID: diskID1},
					{SizeBytes: 40 * conversions.GB, DriveType: "SSD", ID: diskID2},
					{SizeBytes: 20 * conversions.GB, DriveType: "SSD", ID: diskID2},
				}})}
		autoAssignHost = &models.Host{Role: models.HostRoleAutoAssign, InstallationDiskID: diskID1,
			Inventory: Inventory(&InventoryResources{Cpus: 12, Ram: 32 * conversions.GiB,
				Disks: []*models.Disk{
					{SizeBytes: 20 * conversions.GB, DriveType: "HDD", ID: diskID1},
					{SizeBytes: 40 * conversions.GB, DriveType: "SSD", ID: diskID2},
				}})}
	)

	Context("GetHostRequirements", func() {
		table.DescribeTable("compact mode scenario: get requirements for hosts when ", func(cluster *common.Cluster, host *models.Host, expectedResult *models.ClusterHostRequirementsDetails) {
			res, _ := operator.GetHostRequirements(ctx, cluster, host)
			Expect(res).Should(Equal(expectedResult))
		},
			table.Entry("Single master",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk,
				}}},
				masterWithThreeDisk,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUCompactMode + 2*operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBCompactMode + 2*operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("there are three masters",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk,
				}}},
				masterWithThreeDisk,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUCompactMode + 2*operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBCompactMode + 2*operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("there are three masters, with disk not Installation eligible",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDiskSizeOfOneZero, masterWithNoDisk, masterWithOneDisk,
				}}},
				masterWithThreeDiskSizeOfOneZero,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUCompactMode + operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBCompactMode + operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("no disk in one of the master",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk,
				}}},
				masterWithNoDisk,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUCompactMode + operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBCompactMode + operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("no inventory in one of the master",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoInventory, masterWithOneDisk,
				}}},
				masterWithNoInventory,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUCompactMode + operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBCompactMode + operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("only one disk in one of the master",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk,
				}}},
				masterWithOneDisk,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUCompactMode + operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBCompactMode + operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("there are 3 hosts, role of one as auto-assign",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, autoAssignHost,
				}}},
				autoAssignHost,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUCompactMode + operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBCompactMode + operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("there are two master and one worker",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, workerWithTwoDisk,
				}}},
				workerWithTwoDisk,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUStandardMode + operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBStandardMode + operator.config.OCSPerDiskRAMGiB)},
			),
		)

		table.DescribeTable("standard mode scenario: get requirements for hosts when ", func(cluster *common.Cluster, host *models.Host, expectedResult *models.ClusterHostRequirementsDetails) {
			res, _ := operator.GetHostRequirements(ctx, cluster, host)
			Expect(res).Should(Equal(expectedResult))
		},
			table.Entry("there are 4 hosts, role of one as auto-assign",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, autoAssignHost, masterWithOneDisk,
				}}},
				autoAssignHost,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUStandardMode + operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBStandardMode + operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("there are 6 hosts, master requirements",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithNoDisk,
				}}},
				masterWithThreeDisk,
				&models.ClusterHostRequirementsDetails{CPUCores: 0, RAMMib: 0},
			),
			table.Entry("there are 6 hosts, worker with three disk requirements",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithNoDisk,
				}}},
				workerWithThreeDisk,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUStandardMode + 2*operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBStandardMode + 2*operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("there are 6 hosts, worker with three disk requirements and Disk not Installation Eligible",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDiskSizeOfOneZero, workerWithNoDisk,
				}}},
				workerWithThreeDiskSizeOfOneZero,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUStandardMode + operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBStandardMode + operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("there are 6 hosts, worker with two disk requirements",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithNoDisk,
				}}},
				workerWithTwoDisk,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUStandardMode + operator.config.OCSPerDiskCPUCount, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBStandardMode + operator.config.OCSPerDiskRAMGiB)},
			),
			table.Entry("there are 6 hosts, worker with one disk requirements",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithOneDisk,
				}}},
				workerWithOneDisk,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUStandardMode, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBStandardMode)},
			),
			table.Entry("there are 6 hosts, worker with no disk requirements",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithNoDisk,
				}}},
				workerWithNoDisk,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUStandardMode, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBStandardMode)},
			),
			table.Entry("there are 6 hosts, worker with no inventory requirements",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithNoInventory,
				}}},
				workerWithNoInventory,
				&models.ClusterHostRequirementsDetails{CPUCores: operator.config.OCSPerHostCPUStandardMode, RAMMib: conversions.GibToMib(operator.config.OCSPerHostMemoryGiBStandardMode)},
			),
		)
	})

	Context("ValidateHost", func() {
		table.DescribeTable("compact mode scenario: validateHost when ", func(cluster *common.Cluster, host *models.Host, expectedResult api.ValidationResult) {
			res, _ := operator.ValidateHost(ctx, cluster, host)
			Expect(res).Should(Equal(expectedResult))
		},
			table.Entry("Single master",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk,
				}}},
				masterWithThreeDisk,
				api.ValidationResult{Status: api.Success, ValidationId: operator.GetHostValidationID(), Reasons: []string{}},
			),
			table.Entry("there are three masters",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk,
				}}},
				masterWithThreeDisk,
				api.ValidationResult{Status: api.Success, ValidationId: operator.GetHostValidationID(), Reasons: []string{}},
			),
			table.Entry("no disk in one of the master",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk,
				}}},
				masterWithNoDisk,
				api.ValidationResult{Status: api.Failure, ValidationId: operator.GetHostValidationID(), Reasons: []string{"Insufficient disks, OCS requires at least one non-bootable disk on each host in compact mode."}},
			),
			table.Entry("no disk in one of the master",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk,
				}}},
				masterWithNoDisk,
				api.ValidationResult{Status: api.Failure, ValidationId: operator.GetHostValidationID(), Reasons: []string{"Insufficient disks, OCS requires at least one non-bootable disk on each host in compact mode."}},
			),
			table.Entry("only one disk in one of the master",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk,
				}}},
				masterWithOneDisk,
				api.ValidationResult{Status: api.Failure, ValidationId: operator.GetHostValidationID(), Reasons: []string{"Insufficient disks, OCS requires at least one non-bootable disk on each host in compact mode."}},
			),
			table.Entry("master with disk of size zero",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithThreeDiskSizeOfOneZero,
				}}},
				masterWithThreeDiskSizeOfOneZero,
				api.ValidationResult{Status: api.Success, ValidationId: operator.GetHostValidationID(), Reasons: []string{}},
			),
			table.Entry("there are 3 hosts, role of one as auto-assign",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, autoAssignHost,
				}}},
				autoAssignHost,
				api.ValidationResult{Status: api.Success, ValidationId: operator.GetHostValidationID(), Reasons: []string{}},
			),
			table.Entry("there are two master and one worker",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, workerWithTwoDisk,
				}}},
				workerWithTwoDisk,
				api.ValidationResult{Status: api.Failure, ValidationId: operator.GetHostValidationID(), Reasons: []string{"OCS unsupported Host Role for Compact Mode."}},
			),
			table.Entry("there is disk with less size than expected",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithLessDiskSize,
				}}},
				masterWithLessDiskSize,
				api.ValidationResult{Status: api.Failure, ValidationId: operator.GetHostValidationID(), Reasons: []string{"OCS requires all the non-bootable disks to be more than 25 GB"}},
			),
		)

		table.DescribeTable("standard mode scenario: validateHosts when ", func(cluster *common.Cluster, host *models.Host, expectedResult api.ValidationResult) {
			res, _ := operator.ValidateHost(ctx, cluster, host)
			Expect(res).Should(Equal(expectedResult))
		},
			table.Entry("there are 4 hosts, role of one as auto-assign",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, autoAssignHost, masterWithOneDisk,
				}}},
				autoAssignHost,
				api.ValidationResult{Status: api.Failure, ValidationId: operator.GetHostValidationID(), Reasons: []string{"For OCS Standard Mode, host role must be assigned to master or worker."}},
			),
			table.Entry("there are 6 hosts, master",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithNoDisk,
				}}},
				workerWithThreeDisk,
				api.ValidationResult{Status: api.Success, ValidationId: operator.GetHostValidationID(), Reasons: []string{}},
			),
			table.Entry("there are 6 hosts, worker with two disk",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithNoDisk,
				}}},
				workerWithTwoDisk,
				api.ValidationResult{Status: api.Success, ValidationId: operator.GetHostValidationID(), Reasons: []string{}},
			),
			table.Entry("there are 6 hosts, worker with no disk",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithNoDisk,
				}}},
				workerWithNoDisk,
				api.ValidationResult{Status: api.Success, ValidationId: operator.GetHostValidationID(), Reasons: []string{}},
			),
			table.Entry("there are 6 hosts, worker with no inventory",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithNoInventory,
				}}},
				workerWithNoInventory,
				api.ValidationResult{Status: api.Pending, ValidationId: operator.GetHostValidationID(), Reasons: []string{"Missing Inventory in the host."}},
			),
			table.Entry("there is disk with less size than expected",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithLessDiskSize,
				}}},
				workerWithLessDiskSize,
				api.ValidationResult{Status: api.Failure, ValidationId: operator.GetHostValidationID(), Reasons: []string{"OCS requires all the non-bootable disks to be more than 25 GB"}},
			),
			table.Entry("worker with disk of size zero",
				&common.Cluster{Cluster: models.Cluster{Hosts: []*models.Host{
					masterWithThreeDisk, masterWithNoDisk, masterWithOneDisk, workerWithTwoDisk, workerWithThreeDisk, workerWithThreeDiskSizeOfOneZero,
				}}},
				workerWithThreeDiskSizeOfOneZero,
				api.ValidationResult{Status: api.Success, ValidationId: operator.GetHostValidationID(), Reasons: []string{}},
			),
		)
	})

})
