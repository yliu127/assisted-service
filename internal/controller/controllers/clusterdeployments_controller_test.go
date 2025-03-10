package controllers

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/swag"
	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/jinzhu/gorm"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	hiveext "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	"github.com/openshift/assisted-service/internal/bminventory"
	"github.com/openshift/assisted-service/internal/cluster"
	"github.com/openshift/assisted-service/internal/common"
	"github.com/openshift/assisted-service/internal/gencrypto"
	"github.com/openshift/assisted-service/internal/host"
	"github.com/openshift/assisted-service/internal/manifests"
	"github.com/openshift/assisted-service/internal/operators"
	"github.com/openshift/assisted-service/models"
	"github.com/openshift/assisted-service/restapi/operations/installer"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/openshift/hive/apis/hive/v1/aws"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newClusterDeploymentRequest(cluster *hivev1.ClusterDeployment) ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      cluster.ObjectMeta.Name,
			Namespace: cluster.ObjectMeta.Namespace,
		},
	}
}

func newClusterDeployment(name, namespace string, spec hivev1.ClusterDeploymentSpec) *hivev1.ClusterDeployment {
	return &hivev1.ClusterDeployment{
		Spec: spec,
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterDeployment",
			APIVersion: "hive.openshift.io/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func getDefaultSNOAgentClusterInstallSpec(clusterName string) hiveext.AgentClusterInstallSpec {
	return hiveext.AgentClusterInstallSpec{
		Networking: hiveext.Networking{
			MachineNetwork: nil,
			ClusterNetwork: []hiveext.ClusterNetworkEntry{{
				CIDR:       "10.128.0.0/14",
				HostPrefix: 23,
			}},
			ServiceNetwork: []string{"172.30.0.0/16"},
		},
		SSHPublicKey: "some-key",
		ProvisionRequirements: hiveext.ProvisionRequirements{
			ControlPlaneAgents: 1,
			WorkerAgents:       0,
		},
		ImageSetRef:          hivev1.ClusterImageSetReference{Name: "openshift-v4.8.0"},
		ClusterDeploymentRef: corev1.LocalObjectReference{Name: clusterName},
	}
}

func newAgentClusterInstall(name, namespace string, spec hiveext.AgentClusterInstallSpec, cd *hivev1.ClusterDeployment) *hiveext.AgentClusterInstall {
	return &hiveext.AgentClusterInstall{
		Spec: spec,
		TypeMeta: metav1.TypeMeta{
			Kind:       "AgentClusterInstall",
			APIVersion: "hiveextension/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         cd.APIVersion,
				Kind:               cd.Kind,
				Name:               cd.Name,
				UID:                cd.UID,
				BlockOwnerDeletion: swag.Bool(true),
			}},
		},
	}
}

func getDefaultAgentClusterInstallSpec(clusterName string) hiveext.AgentClusterInstallSpec {
	return hiveext.AgentClusterInstallSpec{
		APIVIP:     "1.2.3.8",
		IngressVIP: "1.2.3.9",
		Networking: hiveext.Networking{
			MachineNetwork: nil,
			ClusterNetwork: []hiveext.ClusterNetworkEntry{{
				CIDR:       "10.128.0.0/14",
				HostPrefix: 23,
			}},
			ServiceNetwork: []string{"172.30.0.0/16"},
		},
		SSHPublicKey: "some-key",
		ProvisionRequirements: hiveext.ProvisionRequirements{
			ControlPlaneAgents: 3,
			WorkerAgents:       2,
		},
		ImageSetRef:          hivev1.ClusterImageSetReference{Name: "openshift-v4.8.0"},
		ClusterDeploymentRef: corev1.LocalObjectReference{Name: clusterName},
	}
}

func getDefaultClusterDeploymentSpec(clusterName, aciName, pullSecretName string) hivev1.ClusterDeploymentSpec {
	return hivev1.ClusterDeploymentSpec{
		BaseDomain:  "hive.example.com",
		ClusterName: clusterName,
		PullSecretRef: &corev1.LocalObjectReference{
			Name: pullSecretName,
		},
		ClusterInstallRef: &hivev1.ClusterInstallLocalReference{
			Group:   hiveext.Group,
			Version: hiveext.Version,
			Kind:    "AgentClusterInstall",
			Name:    aciName,
		},
	}
}

func kubeTimeNow() *metav1.Time {
	t := metav1.NewTime(time.Now())
	return &t
}

func simulateACIDeletionWithFinalizer(ctx context.Context, c client.Client, aci *hiveext.AgentClusterInstall) {
	// simulate ACI deletion with finalizer
	aci.ObjectMeta.Finalizers = []string{AgentClusterInstallFinalizerName}
	aci.ObjectMeta.DeletionTimestamp = kubeTimeNow()
	Expect(c.Update(ctx, aci)).Should(BeNil())
}

var _ = Describe("cluster reconcile", func() {
	var (
		c                              client.Client
		cr                             *ClusterDeploymentsReconciler
		ctx                            = context.Background()
		mockCtrl                       *gomock.Controller
		mockInstallerInternal          *bminventory.MockInstallerInternals
		mockClusterApi                 *cluster.MockAPI
		mockHostApi                    *host.MockAPI
		mockManifestsApi               *manifests.MockClusterManifestsInternals
		mockCRDEventsHandler           *MockCRDEventsHandler
		defaultClusterSpec             hivev1.ClusterDeploymentSpec
		clusterName                    = "test-cluster"
		agentClusterInstallName        = "test-cluster-aci"
		defaultAgentClusterInstallSpec hiveext.AgentClusterInstallSpec
		pullSecretName                 = "pull-secret"
		imageSetName                   = "openshift-v4.8.0"
		releaseImage                   = "quay.io/openshift-release-dev/ocp-release:4.8.0-x86_64"
		ocpReleaseVersion              = "4.8.0"
		openshiftVersion               = &models.OpenshiftVersion{
			DisplayName:    new(string),
			ReleaseImage:   new(string),
			ReleaseVersion: &ocpReleaseVersion,
			RhcosImage:     new(string),
			RhcosVersion:   new(string),
			SupportLevel:   new(string),
		}
	)

	getTestCluster := func() *hivev1.ClusterDeployment {
		var cluster hivev1.ClusterDeployment
		key := types.NamespacedName{
			Namespace: testNamespace,
			Name:      clusterName,
		}
		Expect(c.Get(ctx, key, &cluster)).To(BeNil())
		return &cluster
	}

	getTestClusterInstall := func() *hiveext.AgentClusterInstall {
		clusterInstall := &hiveext.AgentClusterInstall{}
		Expect(c.Get(ctx,
			types.NamespacedName{
				Namespace: testNamespace,
				Name:      agentClusterInstallName,
			},
			clusterInstall)).To(BeNil())
		return clusterInstall
	}

	getSecret := func(namespace, name string) *corev1.Secret {
		var secret corev1.Secret
		key := types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		}
		Expect(c.Get(ctx, key, &secret)).To(BeNil())
		return &secret
	}

	BeforeEach(func() {
		defaultClusterSpec = getDefaultClusterDeploymentSpec(clusterName, agentClusterInstallName, pullSecretName)
		defaultAgentClusterInstallSpec = getDefaultAgentClusterInstallSpec(clusterName)
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		mockCtrl = gomock.NewController(GinkgoT())
		mockInstallerInternal = bminventory.NewMockInstallerInternals(mockCtrl)
		mockClusterApi = cluster.NewMockAPI(mockCtrl)
		mockHostApi = host.NewMockAPI(mockCtrl)
		mockCRDEventsHandler = NewMockCRDEventsHandler(mockCtrl)
		mockManifestsApi = manifests.NewMockClusterManifestsInternals(mockCtrl)
		cr = &ClusterDeploymentsReconciler{
			Client:           c,
			APIReader:        c,
			Scheme:           scheme.Scheme,
			Log:              common.GetTestLog(),
			Installer:        mockInstallerInternal,
			ClusterApi:       mockClusterApi,
			HostApi:          mockHostApi,
			CRDEventsHandler: mockCRDEventsHandler,
			Manifests:        mockManifestsApi,
		}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	Context("create cluster", func() {
		BeforeEach(func() {
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(nil, gorm.ErrRecordNotFound)
			pullSecret := getDefaultTestPullSecret("pull-secret", testNamespace)
			Expect(c.Create(ctx, pullSecret)).To(BeNil())
			imageSet := getDefaultTestImageSet(imageSetName, releaseImage)
			Expect(c.Create(ctx, imageSet)).To(BeNil())
		})

		Context("successful creation", func() {
			var clusterReply *common.Cluster

			BeforeEach(func() {
				id := strfmt.UUID(uuid.New().String())
				clusterReply = &common.Cluster{
					Cluster: models.Cluster{
						Status:     swag.String(models.ClusterStatusPendingForInput),
						StatusInfo: swag.String("User input required"),
						ID:         &id,
					},
				}
			})

			validateCreation := func(cluster *hivev1.ClusterDeployment) {
				request := newClusterDeploymentRequest(cluster)
				result, err := cr.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(result).To(Equal(ctrl.Result{}))

				aci := getTestClusterInstall()
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterNotReadyReason))
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterNotReadyMsg))
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionFalse))
			}

			It("create new cluster", func() {
				mockInstallerInternal.EXPECT().RegisterClusterInternal(gomock.Any(), gomock.Any(), gomock.Any(), true).
					Do(func(arg1, arg2 interface{}, params installer.RegisterClusterParams, _ bool) {
						Expect(swag.StringValue(params.NewClusterParams.OpenshiftVersion)).To(Equal(*openshiftVersion.ReleaseVersion))
						Expect(params.NewClusterParams.OcpReleaseImage).To(Equal(*openshiftVersion.ReleaseImage))
					}).Return(clusterReply, nil)
				mockInstallerInternal.EXPECT().AddOpenshiftVersion(gomock.Any(), gomock.Any(), gomock.Any()).Return(openshiftVersion, nil)

				cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
				Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())
				aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
				Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
				validateCreation(cluster)
			})

			It("create sno cluster", func() {
				mockInstallerInternal.EXPECT().RegisterClusterInternal(gomock.Any(), gomock.Any(), gomock.Any(), true).
					Do(func(arg1, arg2 interface{}, params installer.RegisterClusterParams, _ bool) {
						Expect(swag.StringValue(params.NewClusterParams.OpenshiftVersion)).To(Equal(ocpReleaseVersion))
					}).Return(clusterReply, nil)
				mockInstallerInternal.EXPECT().AddOpenshiftVersion(gomock.Any(), gomock.Any(), gomock.Any()).Return(openshiftVersion, nil)

				cluster := newClusterDeployment(clusterName, testNamespace,
					getDefaultClusterDeploymentSpec(clusterName, agentClusterInstallName, pullSecretName))
				Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

				aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, getDefaultSNOAgentClusterInstallSpec(clusterName), cluster)
				Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

				validateCreation(cluster)

			})

			It("create single node cluster", func() {
				mockInstallerInternal.EXPECT().RegisterClusterInternal(gomock.Any(), gomock.Any(), gomock.Any(), true).
					Do(func(ctx, kubeKey interface{}, params installer.RegisterClusterParams, _ bool) {
						Expect(swag.StringValue(params.NewClusterParams.HighAvailabilityMode)).
							To(Equal(HighAvailabilityModeNone))
					}).Return(clusterReply, nil)
				mockInstallerInternal.EXPECT().AddOpenshiftVersion(gomock.Any(), gomock.Any(), gomock.Any()).Return(openshiftVersion, nil)

				cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
				Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

				aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
				aci.Spec.ProvisionRequirements.WorkerAgents = 0
				aci.Spec.ProvisionRequirements.ControlPlaneAgents = 1
				Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

				validateCreation(cluster)
			})

			It("no pull secret name when trying to create a cluster", func() {
				cluster := newClusterDeployment(clusterName, testNamespace,
					getDefaultClusterDeploymentSpec(clusterName, agentClusterInstallName, ""))
				Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

				aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, getDefaultSNOAgentClusterInstallSpec(clusterName), cluster)
				Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

				request := newClusterDeploymentRequest(cluster)
				result, err := cr.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))

				aci = getTestClusterInstall()
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterNotAvailableReason))
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterNotAvailableMsg))
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionUnknown))
				Expect(aci.Status.DebugInfo.State).To(Equal(""))
				Expect(aci.Status.DebugInfo.StateInfo).To(Equal(""))
				Expect(aci.Status.DebugInfo.LogsURL).To(Equal(""))
				Expect(aci.Status.DebugInfo.EventsURL).To(Equal(""))
			})

			It("fail to get openshift version when trying to create a cluster", func() {
				mockInstallerInternal.EXPECT().AddOpenshiftVersion(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, errors.Errorf("some-error"))

				cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
				Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

				aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
				aci.Spec.ProvisionRequirements.WorkerAgents = 0
				aci.Spec.ProvisionRequirements.ControlPlaneAgents = 1
				Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

				request := newClusterDeploymentRequest(cluster)
				result, err := cr.Reconcile(ctx, request)
				Expect(err).To(BeNil())
				Expect(result).To(Equal(ctrl.Result{Requeue: true, RequeueAfter: longerRequeueAfterOnError}))
				aci = getTestClusterInstall()
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterNotAvailableReason))
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterNotAvailableMsg))
				Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionUnknown))
				Expect(aci.Status.DebugInfo.State).To(Equal(""))
				Expect(aci.Status.DebugInfo.StateInfo).To(Equal(""))
				Expect(aci.Status.DebugInfo.LogsURL).To(Equal(""))
				Expect(aci.Status.DebugInfo.EventsURL).To(Equal(""))
			})
		})

		It("create new cluster backend failure", func() {
			errString := "internal error"
			mockInstallerInternal.EXPECT().RegisterClusterInternal(gomock.Any(), gomock.Any(), gomock.Any(), true).
				Return(nil, errors.Errorf(errString))
			mockInstallerInternal.EXPECT().AddOpenshiftVersion(gomock.Any(), gomock.Any(), gomock.Any()).Return(openshiftVersion, nil)

			cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
			Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

			aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, getDefaultSNOAgentClusterInstallSpec(clusterName), cluster)
			Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))

			aci = getTestClusterInstall()
			expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, errString)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
		})
	})

	It("backend internal error when trying to retrieve cluster details", func() {
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(nil, errors.New("internal error"))
		cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)

		Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())
		aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
		Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
		request := newClusterDeploymentRequest(cluster)

		_, err := cr.Reconcile(ctx, request)
		Expect(err).To(BeNil())

		aci = getTestClusterInstall()

		Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
		Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterNotAvailableReason))
		Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterNotAvailableMsg))
		Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionUnknown))
		Expect(aci.Status.DebugInfo.State).To(Equal(""))
		Expect(aci.Status.DebugInfo.StateInfo).To(Equal(""))
		Expect(aci.Status.DebugInfo.LogsURL).To(Equal(""))
		Expect(aci.Status.DebugInfo.EventsURL).To(Equal(""))
	})

	It("not supported platform", func() {
		spec := hivev1.ClusterDeploymentSpec{
			ClusterName: clusterName,
			Provisioning: &hivev1.Provisioning{
				ImageSetRef:            &hivev1.ClusterImageSetReference{Name: imageSetName},
				InstallConfigSecretRef: &corev1.LocalObjectReference{Name: "cluster-install-config"},
			},
			Platform: hivev1.Platform{
				AWS: &aws.Platform{},
			},
			PullSecretRef: &corev1.LocalObjectReference{
				Name: pullSecretName,
			},
		}
		cluster := newClusterDeployment(clusterName, testNamespace, spec)
		cluster.Status = hivev1.ClusterDeploymentStatus{}
		Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

		request := newClusterDeploymentRequest(cluster)
		result, err := cr.Reconcile(ctx, request)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(result).Should(Equal(ctrl.Result{}))
	})

	It("validate owner reference creation", func() {
		sId := strfmt.UUID(uuid.New().String())
		backEndCluster := &common.Cluster{
			Cluster: models.Cluster{
				ID: &sId,
			},
		}
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)

		cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
		Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())
		aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
		aci.ObjectMeta.OwnerReferences = nil
		Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
		request := newClusterDeploymentRequest(cluster)
		_, err := cr.Reconcile(ctx, request)
		Expect(err).ShouldNot(HaveOccurred())
		clusterInstall := &hiveext.AgentClusterInstall{}
		agentClusterInstallKey := types.NamespacedName{
			Namespace: testNamespace,
			Name:      agentClusterInstallName,
		}
		ownref := metav1.OwnerReference{
			APIVersion: cluster.APIVersion,
			Kind:       cluster.Kind,
			Name:       cluster.Name,
			UID:        cluster.UID,
		}
		Expect(c.Get(ctx, agentClusterInstallKey, clusterInstall)).To(BeNil())
		Expect(clusterInstall.ObjectMeta.OwnerReferences).NotTo(BeNil())
		Expect(clusterInstall.ObjectMeta.OwnerReferences[0]).To(Equal(ownref))
	})

	It("validate label on Pull Secret", func() {
		sId := strfmt.UUID(uuid.New().String())
		backEndCluster := &common.Cluster{
			Cluster: models.Cluster{
				ID:                       &sId,
				Name:                     clusterName,
				OpenshiftVersion:         "4.8",
				ClusterNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR,
				ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
				NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
				Status:                   swag.String(models.ClusterStatusReady),
				ServiceNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ServiceNetwork[0],
				IngressVip:               defaultAgentClusterInstallSpec.IngressVIP,
				APIVip:                   defaultAgentClusterInstallSpec.APIVIP,
				BaseDNSDomain:            defaultClusterSpec.BaseDomain,
				SSHPublicKey:             defaultAgentClusterInstallSpec.SSHPublicKey,
				Hyperthreading:           models.ClusterHyperthreadingAll,
				Kind:                     swag.String(models.ClusterKindCluster),
			},
			PullSecret: testPullSecretVal,
		}
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(false, "").Times(1)

		cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
		Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())
		aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
		Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

		pullSecret := getDefaultTestPullSecret(pullSecretName, testNamespace)
		Expect(c.Create(ctx, pullSecret)).To(BeNil())

		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Namespace: testNamespace,
			Name:      pullSecretName,
		}
		Expect(c.Get(ctx, secretKey, secret)).To(BeNil())
		Expect(secret.Labels).To(BeNil())

		request := newClusterDeploymentRequest(cluster)
		result, err := cr.Reconcile(ctx, request)

		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{}))

		Expect(c.Get(ctx, secretKey, secret)).To(BeNil())
		Expect(secret.Labels[WatchResourceLabel]).To(Equal(WatchResourceValue))
	})

	It("validate Event URL", func() {
		_, priv, err := gencrypto.ECDSAKeyPairPEM()
		Expect(err).NotTo(HaveOccurred())
		os.Setenv("EC_PRIVATE_KEY_PEM", priv)
		defer os.Unsetenv("EC_PRIVATE_KEY_PEM")
		Expect(err).NotTo(HaveOccurred())
		serviceBaseURL := "http://acme.com"
		cr.ServiceBaseURL = serviceBaseURL
		sId := strfmt.UUID(uuid.New().String())
		backEndCluster := &common.Cluster{
			Cluster: models.Cluster{
				ID:     &sId,
				Status: swag.String(models.ClusterStatusInsufficient),
			},
		}
		expectedEventUrlPrefix := fmt.Sprintf("%s/api/assisted-install/v1/clusters/%s/events", serviceBaseURL, sId)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)

		cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
		Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())
		aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
		Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
		request := newClusterDeploymentRequest(cluster)
		_, err = cr.Reconcile(ctx, request)
		Expect(err).ShouldNot(HaveOccurred())
		clusterInstall := &hiveext.AgentClusterInstall{}
		agentClusterInstallKey := types.NamespacedName{
			Namespace: testNamespace,
			Name:      agentClusterInstallName,
		}
		Expect(c.Get(ctx, agentClusterInstallKey, clusterInstall)).To(BeNil())
		Expect(clusterInstall.Status.DebugInfo.EventsURL).NotTo(BeNil())
		Expect(clusterInstall.Status.DebugInfo.EventsURL).To(HavePrefix(expectedEventUrlPrefix))
	})

	It("validate Logs URL - before and after host log collection", func() {
		serviceBaseURL := "http://acme.com"
		cr.ServiceBaseURL = serviceBaseURL
		sId := strfmt.UUID(uuid.New().String())
		backEndCluster := &common.Cluster{
			Cluster: models.Cluster{
				ID:     &sId,
				Status: swag.String(models.ClusterStatusInsufficient),
			},
		}
		hosts := make([]*models.Host, 0, 1)
		for i := 0; i < 1; i++ {
			id := strfmt.UUID(uuid.New().String())
			h := &models.Host{
				ID:     &id,
				Status: swag.String(models.HostStatusKnown),
			}
			hosts = append(hosts, h)
		}
		backEndCluster.Hosts = hosts

		expectedLogUrlPrefix := fmt.Sprintf("%s/api/assisted-install/v1/clusters/%s/logs", serviceBaseURL, sId)
		mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&common.Host{Approved: true}, nil).Times(1)
		mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil, errors.Errorf("failed to get host from db")).Times(1)
		mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&common.Host{Approved: true, Host: models.Host{LogsCollectedAt: strfmt.DateTime(time.Now())}}, nil).Times(1)

		By("before installation")
		cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
		Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())
		aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
		Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
		request := newClusterDeploymentRequest(cluster)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		_, err := cr.Reconcile(ctx, request)
		Expect(err).ShouldNot(HaveOccurred())
		clusterInstall := &hiveext.AgentClusterInstall{}
		agentClusterInstallKey := types.NamespacedName{
			Namespace: testNamespace,
			Name:      agentClusterInstallName,
		}
		Expect(c.Get(ctx, agentClusterInstallKey, clusterInstall)).ShouldNot(HaveOccurred())
		Expect(clusterInstall.Status.DebugInfo.LogsURL).To(Equal(""))

		By("failed to get host from db")
		backEndCluster.Status = swag.String(models.ClusterStatusInstalling)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		_, err = cr.Reconcile(ctx, request)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(c.Get(ctx, agentClusterInstallKey, clusterInstall)).ShouldNot(HaveOccurred())
		Expect(clusterInstall.Status.DebugInfo.LogsURL).To(Equal(""))

		By("during installation")
		backEndCluster.Status = swag.String(models.ClusterStatusInstalling)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		_, err = cr.Reconcile(ctx, request)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(c.Get(ctx, agentClusterInstallKey, clusterInstall)).ShouldNot(HaveOccurred())
		Expect(clusterInstall.Status.DebugInfo.LogsURL).To(HavePrefix(expectedLogUrlPrefix))

		By("after installation")
		backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		_, err = cr.Reconcile(ctx, request)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(c.Get(ctx, agentClusterInstallKey, clusterInstall)).ShouldNot(HaveOccurred())
		Expect(clusterInstall.Status.DebugInfo.LogsURL).To(HavePrefix(expectedLogUrlPrefix))
	})

	It("validate Logs URL - before and after controller log collection", func() {
		serviceBaseURL := "http://acme.com"
		cr.ServiceBaseURL = serviceBaseURL
		sId := strfmt.UUID(uuid.New().String())
		backEndCluster := &common.Cluster{
			Cluster: models.Cluster{
				ID:     &sId,
				Status: swag.String(models.ClusterStatusInsufficient),
			},
		}
		expectedLogUrlPrefix := fmt.Sprintf("%s/api/assisted-install/v1/clusters/%s/logs", serviceBaseURL, sId)
		By("before installation")
		cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
		Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())
		aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
		Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
		request := newClusterDeploymentRequest(cluster)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		_, err := cr.Reconcile(ctx, request)
		Expect(err).ShouldNot(HaveOccurred())
		clusterInstall := &hiveext.AgentClusterInstall{}
		agentClusterInstallKey := types.NamespacedName{
			Namespace: testNamespace,
			Name:      agentClusterInstallName,
		}
		Expect(c.Get(ctx, agentClusterInstallKey, clusterInstall)).ShouldNot(HaveOccurred())
		Expect(clusterInstall.Status.DebugInfo.LogsURL).To(Equal(""))
		By("during installation")
		backEndCluster.Status = swag.String(models.ClusterStatusInstalling)
		backEndCluster.ControllerLogsCollectedAt = strfmt.DateTime(time.Now())

		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		_, err = cr.Reconcile(ctx, request)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(c.Get(ctx, agentClusterInstallKey, clusterInstall)).ShouldNot(HaveOccurred())
		Expect(clusterInstall.Status.DebugInfo.LogsURL).To(HavePrefix(expectedLogUrlPrefix))

		By("after installation")
		backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
		_, err = cr.Reconcile(ctx, request)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(c.Get(ctx, agentClusterInstallKey, clusterInstall)).ShouldNot(HaveOccurred())
		Expect(clusterInstall.Status.DebugInfo.LogsURL).To(HavePrefix(expectedLogUrlPrefix))
	})

	It("failed to get cluster from backend", func() {
		cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
		cluster.Status = hivev1.ClusterDeploymentStatus{}
		Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

		aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
		Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

		expectedErr := "expected-error"
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(nil, errors.Errorf(expectedErr))

		request := newClusterDeploymentRequest(cluster)
		result, err := cr.Reconcile(ctx, request)
		Expect(err).To(BeNil())
		Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
		aci = getTestClusterInstall()
		expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, expectedErr)
		Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
		Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
		Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
	})

	It("create cluster without pull secret reference", func() {
		cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
		cluster.Spec.PullSecretRef = nil
		Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())
		aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
		Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
		request := newClusterDeploymentRequest(cluster)

		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(nil, gorm.ErrRecordNotFound)

		_, err := cr.Reconcile(ctx, request)
		Expect(err).To(BeNil())

		aci = getTestClusterInstall()

		Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterInputErrorReason))
		Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
	})

	Context("cluster deletion", func() {
		var (
			sId strfmt.UUID
			cd  *hivev1.ClusterDeployment
			aci *hiveext.AgentClusterInstall
		)

		BeforeEach(func() {
			defaultClusterSpec = getDefaultClusterDeploymentSpec(clusterName, agentClusterInstallName, pullSecretName)
			cd = newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
			cd.Status = hivev1.ClusterDeploymentStatus{}
			defaultAgentClusterInstallSpec = getDefaultAgentClusterInstallSpec(clusterName)
			aci = newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cd)
			id := uuid.New()
			sId = strfmt.UUID(id.String())
			c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
			mockCtrl = gomock.NewController(GinkgoT())
			mockInstallerInternal = bminventory.NewMockInstallerInternals(mockCtrl)
			mockClusterApi = cluster.NewMockAPI(mockCtrl)
			mockHostApi = host.NewMockAPI(mockCtrl)
			mockCRDEventsHandler = NewMockCRDEventsHandler(mockCtrl)
			mockManifestsApi = manifests.NewMockClusterManifestsInternals(mockCtrl)
			cr = &ClusterDeploymentsReconciler{
				Client:           c,
				APIReader:        c,
				Scheme:           scheme.Scheme,
				Log:              common.GetTestLog(),
				Installer:        mockInstallerInternal,
				ClusterApi:       mockClusterApi,
				HostApi:          mockHostApi,
				CRDEventsHandler: mockCRDEventsHandler,
				Manifests:        mockManifestsApi,
			}
			Expect(c.Create(ctx, cd)).ShouldNot(HaveOccurred())
			Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
			pullSecret := getDefaultTestPullSecret("pull-secret", testNamespace)
			Expect(c.Create(ctx, pullSecret)).To(BeNil())
			imageSet := getDefaultTestImageSet(imageSetName, releaseImage)
			Expect(c.Create(ctx, imageSet)).To(BeNil())
		})

		It("agentClusterInstall resource deleted - verify call to deregister cluster", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID: &sId,
				},
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
			mockInstallerInternal.EXPECT().DeregisterClusterInternal(gomock.Any(), gomock.Any()).Return(nil)

			simulateACIDeletionWithFinalizer(ctx, c, aci)
			request := newClusterDeploymentRequest(cd)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(result).Should(Equal(ctrl.Result{}))
		})

		It("agentClusterInstall resource deleted - verify call to cancel installation", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:     &sId,
					Status: swag.String(models.ClusterStatusInstalling),
				},
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
			mockInstallerInternal.EXPECT().DeregisterClusterInternal(gomock.Any(), gomock.Any()).Return(nil)
			mockInstallerInternal.EXPECT().CancelInstallationInternal(gomock.Any(), gomock.Any()).Return(backEndCluster, nil).Times(1)

			simulateACIDeletionWithFinalizer(ctx, c, aci)
			request := newClusterDeploymentRequest(cd)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(result).Should(Equal(ctrl.Result{}))
		})

		It("cluster deregister failed - internal error", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID: &sId,
				},
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
			mockInstallerInternal.EXPECT().DeregisterClusterInternal(gomock.Any(), gomock.Any()).Return(errors.New("internal error"))

			expectedErrMsg := fmt.Sprintf("failed to deregister cluster: %s: internal error", cd.Name)

			simulateACIDeletionWithFinalizer(ctx, c, aci)
			Expect(c.Update(ctx, aci)).Should(BeNil())
			request := newClusterDeploymentRequest(cd)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(Equal(expectedErrMsg))
			Expect(result).Should(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
		})

		It("agentClusterInstall resource deleted and created again", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID: &sId,
				},
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
			mockInstallerInternal.EXPECT().DeregisterClusterInternal(gomock.Any(), gomock.Any()).Return(nil)
			mockInstallerInternal.EXPECT().AddOpenshiftVersion(gomock.Any(), gomock.Any(), gomock.Any()).Return(openshiftVersion, nil)

			simulateACIDeletionWithFinalizer(ctx, c, aci)
			request := newClusterDeploymentRequest(cd)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).ShouldNot(HaveOccurred())
			Expect(result).Should(Equal(ctrl.Result{}))

			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(nil, gorm.ErrRecordNotFound)
			mockInstallerInternal.EXPECT().RegisterClusterInternal(gomock.Any(), gomock.Any(), gomock.Any(), true).Return(backEndCluster, nil)

			aci = newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cd)
			Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

			request = newClusterDeploymentRequest(cd)
			result, err = cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("cluster installation", func() {
		var (
			sId            strfmt.UUID
			cluster        *hivev1.ClusterDeployment
			aci            *hiveext.AgentClusterInstall
			backEndCluster *common.Cluster
		)

		BeforeEach(func() {
			pullSecret := getDefaultTestPullSecret("pull-secret", testNamespace)
			Expect(c.Create(ctx, pullSecret)).To(BeNil())
			imageSet := getDefaultTestImageSet(imageSetName, releaseImage)
			Expect(c.Create(ctx, imageSet)).To(BeNil())
			cluster = newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
			id := uuid.New()
			sId = strfmt.UUID(id.String())
			Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())
			aci = newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
			Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
			backEndCluster = &common.Cluster{
				Cluster: models.Cluster{
					ID:                       &sId,
					Name:                     clusterName,
					OpenshiftVersion:         "4.8",
					ClusterNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR,
					ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
					NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:                   swag.String(models.ClusterStatusReady),
					ServiceNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ServiceNetwork[0],
					IngressVip:               defaultAgentClusterInstallSpec.IngressVIP,
					APIVip:                   defaultAgentClusterInstallSpec.APIVIP,
					BaseDNSDomain:            defaultClusterSpec.BaseDomain,
					SSHPublicKey:             defaultAgentClusterInstallSpec.SSHPublicKey,
					Hyperthreading:           models.ClusterHyperthreadingAll,
					Kind:                     swag.String(models.ClusterKindCluster),
				},
				PullSecret: testPullSecretVal,
			}
			hosts := make([]*models.Host, 0, 5)
			for i := 0; i < 5; i++ {
				id := strfmt.UUID(uuid.New().String())
				h := &models.Host{
					ID:     &id,
					Status: swag.String(models.HostStatusKnown),
				}
				hosts = append(hosts, h)
			}
			backEndCluster.Hosts = hosts
		})

		It("success", func() {
			backEndCluster.Status = swag.String(models.ClusterStatusReady)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(5)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(5)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(models.ListManifests{}, nil).Times(1)

			installClusterReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:         backEndCluster.ID,
					Status:     swag.String(models.ClusterStatusPreparingForInstallation),
					StatusInfo: swag.String("Waiting for control plane"),
				},
			}
			mockInstallerInternal.EXPECT().InstallClusterInternal(gomock.Any(), gomock.Any()).
				Return(installClusterReply, nil)

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstallationInProgressReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Message).To(Equal(hiveext.ClusterInstallationInProgressMsg + " Waiting for control plane"))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("hold installation", func() {
			backEndCluster.Status = swag.String(models.ClusterStatusReady)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(10)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(15)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(models.ListManifests{}, nil).Times(1)

			installClusterReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:         backEndCluster.ID,
					Status:     swag.String(models.ClusterStatusPreparingForInstallation),
					StatusInfo: swag.String("Waiting for control plane"),
				},
			}
			mockInstallerInternal.EXPECT().InstallClusterInternal(gomock.Any(), gomock.Any()).
				Return(installClusterReply, nil)

			By("hold installation")
			aci = getTestClusterInstall()
			aci.Spec.HoldInstallation = true
			Expect(c.Update(ctx, aci)).To(BeNil())

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstallationOnHoldReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Message).To(Equal(hiveext.ClusterInstallationOnHoldMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionFalse))

			By("unhold installation")
			aci.Spec.HoldInstallation = false
			Expect(c.Update(ctx, aci)).To(BeNil())

			request = newClusterDeploymentRequest(cluster)
			result, err = cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstallationInProgressReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Message).To(Equal(hiveext.ClusterInstallationInProgressMsg + " Waiting for control plane"))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("CVO status", func() {
			backEndCluster.Status = swag.String(models.ClusterStatusReady)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(5)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(5)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(models.ListManifests{}, nil).Times(1)

			cvoStatusInfo := "Working towards 4.8.0-rc.0: 654 of 676 done (96% complete)"
			oper := make([]*models.MonitoredOperator, 1)
			oper[0] = &models.MonitoredOperator{
				OperatorType: models.OperatorTypeBuiltin,
				Name:         operators.OperatorCVO.Name,
				Status:       models.OperatorStatusProgressing,
				StatusInfo:   cvoStatusInfo,
			}
			installClusterReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:                 backEndCluster.ID,
					Status:             swag.String(models.ClusterStatusFinalizing),
					StatusInfo:         swag.String("Finalizing cluster installation"),
					MonitoredOperators: oper,
				},
			}
			mockInstallerInternal.EXPECT().InstallClusterInternal(gomock.Any(), gomock.Any()).
				Return(installClusterReply, nil)

			cvoMsg := fmt.Sprintf(". Cluster version status: %s, message: %s", models.OperatorStatusProgressing, cvoStatusInfo)
			expectedMsg := fmt.Sprintf("%s %s%s", hiveext.ClusterInstallationInProgressMsg, *installClusterReply.StatusInfo, cvoMsg)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstallationInProgressReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Message).To(Equal(expectedMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("CVO empty status", func() {
			backEndCluster.Status = swag.String(models.ClusterStatusReady)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(5)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(5)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(models.ListManifests{}, nil).Times(1)

			oper := make([]*models.MonitoredOperator, 1)
			oper[0] = &models.MonitoredOperator{
				OperatorType: models.OperatorTypeBuiltin,
				Name:         operators.OperatorCVO.Name,
			}
			installClusterReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:                 backEndCluster.ID,
					Status:             swag.String(models.ClusterStatusFinalizing),
					StatusInfo:         swag.String("Finalizing cluster installation"),
					MonitoredOperators: oper,
				},
			}
			mockInstallerInternal.EXPECT().InstallClusterInternal(gomock.Any(), gomock.Any()).
				Return(installClusterReply, nil)

			expectedMsg := fmt.Sprintf("%s %s", hiveext.ClusterInstallationInProgressMsg, *installClusterReply.StatusInfo)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstallationInProgressReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Message).To(Equal(expectedMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("installed no day2 flag", func() {
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindCluster)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
			password := "test"
			username := "admin"
			kubeconfig := "kubeconfig content"
			cred := &models.Credentials{
				Password: password,
				Username: username,
			}
			mockInstallerInternal.EXPECT().GetCredentialsInternal(gomock.Any(), gomock.Any()).Return(cred, nil).Times(1)
			mockInstallerInternal.EXPECT().DownloadClusterFilesInternal(gomock.Any(), gomock.Any()).Return(ioutil.NopCloser(strings.NewReader(kubeconfig)), int64(len(kubeconfig)), nil).Times(1)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			cluster = getTestCluster()
			Expect(aci.Spec.ClusterMetadata.ClusterID).To(Equal(openshiftID.String()))
			secretAdmin := getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminPasswordSecretRef.Name)
			Expect(string(secretAdmin.Data["password"])).To(Equal(password))
			Expect(string(secretAdmin.Data["username"])).To(Equal(username))
			secretKubeConfig := getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminKubeconfigSecretRef.Name)
			Expect(string(secretKubeConfig.Data["kubeconfig"])).To(Equal(kubeconfig))

			By("Call reconcile again to test no delete of day1 cluster")
			result, err = cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("installed with day2 flag", func() {
			cr.EnableDay2Cluster = true
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindCluster)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
			password := "test"
			username := "admin"
			kubeconfig := "kubeconfig content"
			cred := &models.Credentials{
				Password: password,
				Username: username,
			}

			mockInstallerInternal.EXPECT().GetCredentialsInternal(gomock.Any(), gomock.Any()).Return(cred, nil).Times(1)
			mockInstallerInternal.EXPECT().DownloadClusterFilesInternal(gomock.Any(), gomock.Any()).Return(ioutil.NopCloser(strings.NewReader(kubeconfig)), int64(len(kubeconfig)), nil).Times(1)
			mockInstallerInternal.EXPECT().TransformClusterToDay2Internal(gomock.Any(), gomock.Any()).Times(1)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			cluster = getTestCluster()
			Expect(aci.Spec.ClusterMetadata.ClusterID).To(Equal(openshiftID.String()))
			secretAdmin := getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminPasswordSecretRef.Name)
			Expect(string(secretAdmin.Data["password"])).To(Equal(password))
			Expect(string(secretAdmin.Data["username"])).To(Equal(username))
			secretKubeConfig := getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminKubeconfigSecretRef.Name)
			Expect(string(secretKubeConfig.Data["kubeconfig"])).To(Equal(kubeconfig))

			By("Call reconcile again to test transform into day2 cluster")
			result, err = cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("installed with day2 flag set to false", func() {
			cr.EnableDay2Cluster = false
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindCluster)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
			password := "test"
			username := "admin"
			kubeconfig := "kubeconfig content"
			cred := &models.Credentials{
				Password: password,
				Username: username,
			}

			mockInstallerInternal.EXPECT().GetCredentialsInternal(gomock.Any(), gomock.Any()).Return(cred, nil).Times(1)
			mockInstallerInternal.EXPECT().DownloadClusterFilesInternal(gomock.Any(), gomock.Any()).Return(ioutil.NopCloser(strings.NewReader(kubeconfig)), int64(len(kubeconfig)), nil).Times(1)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			cluster = getTestCluster()
			Expect(aci.Spec.ClusterMetadata.ClusterID).To(Equal(openshiftID.String()))
			secretAdmin := getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminPasswordSecretRef.Name)
			Expect(string(secretAdmin.Data["password"])).To(Equal(password))
			Expect(string(secretAdmin.Data["username"])).To(Equal(username))
			secretKubeConfig := getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminKubeconfigSecretRef.Name)
			Expect(string(secretKubeConfig.Data["kubeconfig"])).To(Equal(kubeconfig))
		})

		It("Reconcile to upgrade a day1 to day2 cluster", func() {
			By("Create a Day1 cluster")
			cr.EnableDay2Cluster = false
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindCluster)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
			kubeconfig := "kubeconfig content"
			mockInstallerInternal.EXPECT().GetCredentialsInternal(gomock.Any(), gomock.Any()).Return(&models.Credentials{Password: "foo", Username: "bar"}, nil).Times(1)
			mockInstallerInternal.EXPECT().DownloadClusterFilesInternal(gomock.Any(), gomock.Any()).Return(ioutil.NopCloser(strings.NewReader(kubeconfig)), int64(len(kubeconfig)), nil).Times(1)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
			aci = getTestClusterInstall()
			Expect(aci.Status.DebugInfo.State).To(Equal(models.ClusterStatusInstalled))

			By("Reconcile to transform into day2 cluster")
			day2backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:               backEndCluster.ID,
					Name:             clusterName,
					OpenshiftVersion: "4.8",
					Status:           swag.String(models.ClusterStatusAddingHosts),
					APIVip:           backEndCluster.APIVip,
					BaseDNSDomain:    backEndCluster.BaseDNSDomain,
					Kind:             swag.String(models.ClusterKindAddHostsCluster),
					APIVipDNSName:    swag.String(fmt.Sprintf("api.%s.%s", backEndCluster.Name, backEndCluster.BaseDNSDomain)),
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().TransformClusterToDay2Internal(gomock.Any(), gomock.Any()).Times(1).Return(day2backEndCluster, nil)
			cr.EnableDay2Cluster = true
			result, err = cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
			aci = getTestClusterInstall()
			Expect(aci.Status.DebugInfo.State).To(Equal(models.ClusterStatusAddingHosts))
		})

		It("update kubeconfig ingress", func() {
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalling)
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindCluster)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(false, "").Times(1)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(5)
			password := "test"
			username := "admin"
			kubeconfigNoIngress := "kubeconfig content NOINGRESS"
			cred := &models.Credentials{
				Password: password,
				Username: username,
			}
			mockInstallerInternal.EXPECT().GetCredentialsInternal(gomock.Any(), gomock.Any()).Return(cred, nil).Times(1)
			mockInstallerInternal.EXPECT().DownloadClusterFilesInternal(gomock.Any(), gomock.Any()).Return(ioutil.NopCloser(strings.NewReader(kubeconfigNoIngress)), int64(len(kubeconfigNoIngress)), nil).Times(1)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			cluster = getTestCluster()
			secretKubeConfig := getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminKubeconfigSecretRef.Name)
			Expect(string(secretKubeConfig.Data["kubeconfig"])).To(Equal(kubeconfigNoIngress))
			Expect(aci.Spec.ClusterMetadata.ClusterID).To(Equal(openshiftID.String()))

			By("Call reconcile again to test update of Kubeconfig secret")
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			kubeconfigIngress := "kubeconfig content WITHINGRESS"
			mockInstallerInternal.EXPECT().DownloadClusterFilesInternal(gomock.Any(), gomock.Any()).Return(ioutil.NopCloser(strings.NewReader(kubeconfigIngress)), int64(len(kubeconfigIngress)), nil).Times(1)
			result, err = cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
			aci = getTestClusterInstall()
			cluster = getTestCluster()
			secretAdmin := getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminPasswordSecretRef.Name)
			Expect(string(secretAdmin.Data["password"])).To(Equal(password))
			Expect(string(secretAdmin.Data["username"])).To(Equal(username))
			secretKubeConfig = getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminKubeconfigSecretRef.Name)
			Expect(string(secretKubeConfig.Data["kubeconfig"])).To(Equal(kubeconfigIngress))
		})

		It("installed SNO no day2", func() {
			cr.EnableDay2Cluster = true
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			backEndCluster.StatusInfo = swag.String("Done")
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindCluster)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(2)
			password := "test"
			username := "admin"
			kubeconfig := "kubeconfig content"
			cred := &models.Credentials{
				Password: password,
				Username: username,
			}
			mockInstallerInternal.EXPECT().GetCredentialsInternal(gomock.Any(), gomock.Any()).Return(cred, nil).Times(1)
			mockInstallerInternal.EXPECT().DownloadClusterFilesInternal(gomock.Any(), gomock.Any()).Return(ioutil.NopCloser(strings.NewReader(kubeconfig)), int64(len(kubeconfig)), nil).Times(1)
			aci.Spec.ProvisionRequirements.WorkerAgents = 0
			aci.Spec.ProvisionRequirements.ControlPlaneAgents = 1
			cluster.Spec.BaseDomain = "hive.example.com"
			Expect(c.Update(ctx, cluster)).Should(BeNil())
			Expect(c.Update(ctx, aci)).Should(BeNil())
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstalledReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Message).To(Equal(hiveext.ClusterInstalledMsg + " Done"))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionTrue))

			cluster = getTestCluster()
			Expect(aci.Spec.ClusterMetadata.ClusterID).To(Equal(openshiftID.String()))
			secretAdmin := getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminPasswordSecretRef.Name)
			Expect(string(secretAdmin.Data["password"])).To(Equal(password))
			Expect(string(secretAdmin.Data["username"])).To(Equal(username))
			secretKubeConfig := getSecret(cluster.Namespace, aci.Spec.ClusterMetadata.AdminKubeconfigSecretRef.Name)
			Expect(string(secretKubeConfig.Data["kubeconfig"])).To(Equal(kubeconfig))

			By("Call reconcile again to test delete of day1 cluster")
			result, err = cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("Fail to transform into day2", func() {
			cr.EnableDay2Cluster = true
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindCluster)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
			expectedErr := "internal error"
			mockInstallerInternal.EXPECT().TransformClusterToDay2Internal(gomock.Any(), gomock.Any()).Times(1).Return(nil, errors.New(expectedErr))
			setClusterCondition(&aci.Status.Conditions, hivev1.ClusterInstallCondition{
				Type:    hiveext.ClusterCompletedCondition,
				Status:  corev1.ConditionTrue,
				Reason:  hiveext.ClusterInstalledReason,
				Message: hiveext.ClusterInstalledMsg,
			})
			Expect(c.Status().Update(ctx, aci)).Should(BeNil())
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))

			aci = getTestClusterInstall()
			expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, expectedErr)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterNotAvailableReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionUnknown))
		})

		It("Create day2 if day1 is already deleted none SNO", func() {
			cr.EnableDay2Cluster = true
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(nil, gorm.ErrRecordNotFound)
			id := strfmt.UUID(uuid.New().String())
			clusterReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:     &id,
					Status: swag.String(models.ClusterStatusAddingHosts),
				},
			}
			mockInstallerInternal.EXPECT().RegisterAddHostsClusterInternal(gomock.Any(), gomock.Any(), gomock.Any(), true).Return(clusterReply, nil)
			mockInstallerInternal.EXPECT().AddOpenshiftVersion(gomock.Any(), gomock.Any(), gomock.Any()).Return(openshiftVersion, nil)
			setClusterCondition(&aci.Status.Conditions, hivev1.ClusterInstallCondition{
				Type:    hiveext.ClusterCompletedCondition,
				Status:  corev1.ConditionTrue,
				Reason:  hiveext.ClusterInstalledReason,
				Message: hiveext.ClusterInstalledMsg,
			})
			Expect(c.Status().Update(ctx, aci)).Should(BeNil())
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
		})

		It("installed - fail to get kube config", func() {
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindCluster)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
			password := "test"
			username := "admin"
			cred := &models.Credentials{
				Password: password,
				Username: username,
			}
			mockInstallerInternal.EXPECT().GetCredentialsInternal(gomock.Any(), gomock.Any()).Return(cred, nil).Times(1)
			expectedErr := "internal error"
			mockInstallerInternal.EXPECT().DownloadClusterFilesInternal(gomock.Any(), gomock.Any()).Return(nil, int64(0), errors.New(expectedErr)).Times(1)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))

			aci = getTestClusterInstall()
			expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, expectedErr)
			Expect(cluster.Spec.ClusterMetadata).To(BeNil())
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstalledReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionTrue))
		})

		It("installed - fail to get admin password", func() {
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindCluster)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
			expectedErr := "internal error"
			mockInstallerInternal.EXPECT().GetCredentialsInternal(gomock.Any(), gomock.Any()).Return(nil, errors.New(expectedErr)).Times(1)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))

			aci = getTestClusterInstall()
			Expect(cluster.Spec.ClusterMetadata).To(BeNil())
			expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, expectedErr)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstalledReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionTrue))
		})

		It("failed to start installation", func() {
			expectedErr := "internal error"
			backEndCluster.Status = swag.String(models.ClusterStatusReady)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockInstallerInternal.EXPECT().InstallClusterInternal(gomock.Any(), gomock.Any()).
				Return(nil, errors.Errorf(expectedErr))
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(10)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(15)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(models.ListManifests{}, nil).Times(1)

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))

			aci = getTestClusterInstall()
			expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, expectedErr)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterReadyReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterReadyMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionTrue))
		})

		It("not ready for installation", func() {
			backEndCluster.Status = swag.String(models.ClusterStatusPendingForInput)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(false, "").Times(1)
			Expect(c.Update(ctx, cluster)).Should(BeNil())
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: false}, nil).Times(5)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterNotReadyReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterNotReadyMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("not ready for installation - hosts not approved", func() {
			backEndCluster.Status = swag.String(models.ClusterStatusPendingForInput)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(5)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: false}, nil).Times(10)

			Expect(c.Update(ctx, cluster)).Should(BeNil())
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterNotReadyReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterNotReadyMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("ready for installation - but not all hosts are approved", func() {
			backEndCluster.Status = swag.String(models.ClusterStatusReady)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(10)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: false}, nil).Times(15)

			Expect(c.Update(ctx, cluster)).Should(BeNil())
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			expectedHosts := defaultAgentClusterInstallSpec.ProvisionRequirements.ControlPlaneAgents +
				defaultAgentClusterInstallSpec.ProvisionRequirements.WorkerAgents
			msg := fmt.Sprintf(hiveext.ClusterUnapprovedAgentsMsg, expectedHosts)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterUnapprovedAgentsReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(msg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("ready for installation - but not all hosts are ready", func() {
			backEndCluster.Status = swag.String(models.ClusterStatusReady)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: false}, nil).Times(5)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(false).Times(10)

			Expect(c.Update(ctx, cluster)).Should(BeNil())
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			expectedHosts := defaultAgentClusterInstallSpec.ProvisionRequirements.ControlPlaneAgents +
				defaultAgentClusterInstallSpec.ProvisionRequirements.WorkerAgents
			msg := fmt.Sprintf(hiveext.ClusterInsufficientAgentsMsg, expectedHosts, 0)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterInsufficientAgentsReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(msg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("ready for installation - but too much approved hosts", func() {
			backEndCluster.Status = swag.String(models.ClusterStatusReady)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(10)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(15)

			aci.Spec.ProvisionRequirements.WorkerAgents = 0
			Expect(c.Update(ctx, aci)).Should(BeNil())
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			expectedHosts := aci.Spec.ProvisionRequirements.ControlPlaneAgents + aci.Spec.ProvisionRequirements.WorkerAgents
			msg := fmt.Sprintf(hiveext.ClusterAdditionalAgentsMsg, expectedHosts, 5)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterAdditionalAgentsReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(msg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("ready for installation - but too much registered hosts", func() {
			backEndCluster.Status = swag.String(models.ClusterStatusReady)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(10)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(3)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: false}, nil).Times(2)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(3)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: false}, nil).Times(2)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(5)

			aci.Spec.ProvisionRequirements.WorkerAgents = 0
			Expect(c.Update(ctx, aci)).Should(BeNil())
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			expectedHosts := aci.Spec.ProvisionRequirements.ControlPlaneAgents + aci.Spec.ProvisionRequirements.WorkerAgents
			msg := fmt.Sprintf(hiveext.ClusterAdditionalAgentsMsg, expectedHosts, 5)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterAdditionalAgentsReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(msg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("install day2 host", func() {
			cr.EnableDay2Cluster = true
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindAddHostsCluster)
			backEndCluster.Status = swag.String(models.ClusterStatusAddingHosts)
			id := strfmt.UUID(uuid.New().String())
			h := &models.Host{
				ID:     &id,
				Status: swag.String(models.HostStatusKnown),
			}

			By("hold installation should not affect day2")
			aci = getTestClusterInstall()
			aci.Spec.HoldInstallation = true
			Expect(c.Update(ctx, aci)).To(BeNil())

			backEndCluster.Hosts = []*models.Host{h}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(2)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(1)
			mockInstallerInternal.EXPECT().InstallSingleDay2HostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterAlreadyInstallingReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterAlreadyInstallingMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionTrue))
		})

		It("install failure day2 host", func() {
			cr.EnableDay2Cluster = true
			openshiftID := strfmt.UUID(uuid.New().String())
			backEndCluster.Status = swag.String(models.ClusterStatusInstalled)
			backEndCluster.OpenshiftClusterID = openshiftID
			backEndCluster.Kind = swag.String(models.ClusterKindAddHostsCluster)
			backEndCluster.Status = swag.String(models.ClusterStatusAddingHosts)
			id := strfmt.UUID(uuid.New().String())
			h := &models.Host{
				ID:     &id,
				Status: swag.String(models.HostStatusKnown),
			}
			backEndCluster.Hosts = []*models.Host{h}
			expectedErr := "internal error"
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil).Times(1)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(2)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(1)
			mockInstallerInternal.EXPECT().InstallSingleDay2HostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(errors.New(expectedErr))
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))

			aci = getTestClusterInstall()
			expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, expectedErr)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterAlreadyInstallingReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterAlreadyInstallingMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionTrue))
		})

		It("Install with manifests - no configmap", func() {
			aci.Spec.ManifestsConfigMapRef = &corev1.LocalObjectReference{Name: "cluster-install-config"}
			Expect(c.Update(ctx, aci)).Should(BeNil())

			backEndCluster.Status = swag.String(models.ClusterStatusReady)
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(10)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(15)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(models.ListManifests{}, nil).Times(1)

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{Requeue: true, RequeueAfter: longerRequeueAfterOnError}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).NotTo(Equal(""))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterReadyReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterReadyMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionTrue))
		})

		It("Update manifests - manifests exists , create failed", func() {
			ref := &corev1.LocalObjectReference{Name: "cluster-install-config"}
			data := map[string]string{"test.yaml": "test"}
			cm := &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					Kind:       "ConfigMap",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: cluster.ObjectMeta.Namespace,
					Name:      "cluster-install-config",
				},
				Data: data,
			}
			Expect(c.Create(ctx, cm)).To(BeNil())

			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(10)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(15)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(models.ListManifests{}, nil).Times(1)
			mockManifestsApi.EXPECT().CreateClusterManifestInternal(gomock.Any(), gomock.Any()).Return(nil, errors.Errorf("error")).Times(1)
			request := newClusterDeploymentRequest(cluster)
			aci = getTestClusterInstall()
			aci.Spec.ManifestsConfigMapRef = ref
			Expect(c.Update(ctx, aci)).Should(BeNil())
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{Requeue: true, RequeueAfter: longerRequeueAfterOnError}))

			aci = getTestClusterInstall()
			expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, "error")
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterReadyReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterReadyMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionTrue))
		})

		It("Update manifests - manifests exists , list failed", func() {
			ref := &corev1.LocalObjectReference{Name: "cluster-install-config"}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(10)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(15)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(nil, errors.Errorf("error")).Times(1)

			request := newClusterDeploymentRequest(cluster)
			cluster = getTestCluster()
			aci.Spec.ManifestsConfigMapRef = ref
			Expect(c.Update(ctx, aci)).Should(BeNil())
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{Requeue: true, RequeueAfter: longerRequeueAfterOnError}))

			aci = getTestClusterInstall()
			expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, "error")
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterReadyReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterReadyMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionTrue))
		})

		It("Update manifests - succeed", func() {
			ref := &corev1.LocalObjectReference{Name: "cluster-install-config"}
			data := map[string]string{"test.yaml": "test"}
			cm := &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					Kind:       "ConfigMap",
					APIVersion: "v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: cluster.ObjectMeta.Namespace,
					Name:      "cluster-install-config",
				},
				Data: data,
			}
			Expect(c.Create(ctx, cm)).To(BeNil())

			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockManifestsApi.EXPECT().CreateClusterManifestInternal(gomock.Any(), gomock.Any()).Return(nil, nil).Times(1)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(models.ListManifests{}, nil).Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(5)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(5)
			installClusterReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:         backEndCluster.ID,
					Status:     swag.String(models.ClusterStatusPreparingForInstallation),
					StatusInfo: swag.String("Waiting for control plane"),
				},
			}
			mockInstallerInternal.EXPECT().InstallClusterInternal(gomock.Any(), gomock.Any()).
				Return(installClusterReply, nil)

			cluster = getTestCluster()
			aci.Spec.ManifestsConfigMapRef = ref
			Expect(c.Update(ctx, aci)).Should(BeNil())
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstallationInProgressReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Message).To(Equal(hiveext.ClusterInstallationInProgressMsg + " Waiting for control plane"))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("Update manifests - no manifests", func() {

			installClusterReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:         backEndCluster.ID,
					Status:     swag.String(models.ClusterStatusPreparingForInstallation),
					StatusInfo: swag.String("Waiting for control plane"),
				},
			}
			mockInstallerInternal.EXPECT().InstallClusterInternal(gomock.Any(), gomock.Any()).
				Return(installClusterReply, nil).Times(1)

			By("no manifests")
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(nil, nil).Times(1)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(5)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(5)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstallationInProgressReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Message).To(Equal(hiveext.ClusterInstallationInProgressMsg + " Waiting for control plane"))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("Update manifests - delete old + error should be ignored", func() {
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockManifestsApi.EXPECT().ListClusterManifestsInternal(gomock.Any(), gomock.Any()).Return(models.ListManifests{&models.Manifest{FileName: "test", Folder: "test"}, &models.Manifest{FileName: "test2", Folder: "test2"}}, nil).Times(1)
			mockManifestsApi.EXPECT().DeleteClusterManifestInternal(gomock.Any(), gomock.Any()).Return(nil).Times(1)
			mockManifestsApi.EXPECT().DeleteClusterManifestInternal(gomock.Any(), gomock.Any()).Return(errors.Errorf("ignore it")).Times(1)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(true, "").Times(1)
			mockHostApi.EXPECT().IsInstallable(gomock.Any()).Return(true).Times(5)
			mockInstallerInternal.EXPECT().GetCommonHostInternal(gomock.Any(), gomock.Any(), gomock.Any()).Return(&common.Host{Approved: true}, nil).Times(5)
			installClusterReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:         backEndCluster.ID,
					Status:     swag.String(models.ClusterStatusPreparingForInstallation),
					StatusInfo: swag.String("Waiting for control plane"),
				},
			}
			mockInstallerInternal.EXPECT().InstallClusterInternal(gomock.Any(), gomock.Any()).
				Return(installClusterReply, nil)

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Reason).To(Equal(hiveext.ClusterInstallationInProgressReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Message).To(Equal(hiveext.ClusterInstallationInProgressMsg + " Waiting for control plane"))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterCompletedCondition).Status).To(Equal(corev1.ConditionFalse))

		})
	})

	It("reconcile on installed sno cluster should not return an error or requeue", func() {
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(nil, gorm.ErrRecordNotFound).Times(1)
		cluster := newClusterDeployment(clusterName, testNamespace,
			getDefaultClusterDeploymentSpec(clusterName, agentClusterInstallName, pullSecretName))
		Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

		aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, getDefaultSNOAgentClusterInstallSpec(clusterName), cluster)
		Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

		request := newClusterDeploymentRequest(cluster)
		result, err := cr.Reconcile(ctx, request)
		Expect(err).To(BeNil())
		Expect(result.Requeue).To(BeFalse())
	})

	Context("cluster update", func() {
		var (
			sId     strfmt.UUID
			cluster *hivev1.ClusterDeployment
			aci     *hiveext.AgentClusterInstall
		)

		BeforeEach(func() {
			pullSecret := getDefaultTestPullSecret("pull-secret", testNamespace)
			Expect(c.Create(ctx, pullSecret)).To(BeNil())

			cluster = newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
			id := uuid.New()
			sId = strfmt.UUID(id.String())

			Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

			aci = newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
			Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
		})

		It("update pull-secret network cidr and cluster name", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:                       &sId,
					Name:                     "different-cluster-name",
					OpenshiftVersion:         "4.8",
					ClusterNetworkCidr:       "11.129.0.0/14",
					ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
					NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:                   swag.String(models.ClusterStatusPendingForInput),
				},
				PullSecret: "different-pull-secret",
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			updateReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:         &sId,
					Status:     swag.String(models.ClusterStatusInsufficient),
					StatusInfo: swag.String(models.ClusterStatusInsufficient),
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().UpdateClusterNonInteractive(gomock.Any(), gomock.Any()).
				Do(func(ctx context.Context, param installer.UpdateClusterParams) {
					Expect(swag.StringValue(param.ClusterUpdateParams.PullSecret)).To(Equal(testPullSecretVal))
					Expect(swag.StringValue(param.ClusterUpdateParams.Name)).To(Equal(defaultClusterSpec.ClusterName))
					Expect(swag.StringValue(param.ClusterUpdateParams.ClusterNetworkCidr)).
						To(Equal(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR))
				}).Return(updateReply, nil)

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterSyncedOkReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Reason).To(Equal(hiveext.ClusterNotReadyReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterNotReadyMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("only state changed", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:                       &sId,
					Name:                     clusterName,
					OpenshiftVersion:         "4.8",
					ClusterNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR,
					ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
					NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:                   swag.String(models.ClusterStatusInsufficient),
					ServiceNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ServiceNetwork[0],
					IngressVip:               defaultAgentClusterInstallSpec.IngressVIP,
					APIVip:                   defaultAgentClusterInstallSpec.APIVIP,
					BaseDNSDomain:            defaultClusterSpec.BaseDomain,
					SSHPublicKey:             defaultAgentClusterInstallSpec.SSHPublicKey,
					Hyperthreading:           models.ClusterHyperthreadingAll,
					Kind:                     swag.String(models.ClusterKindCluster),
					ValidationsInfo:          "{\"some-check\":[{\"id\":\"checking1\",\"status\":\"failure\",\"message\":\"Check1 is not OK\"},{\"id\":\"checking2\",\"status\":\"success\",\"message\":\"Check2 is OK\"},{\"id\":\"checking3\",\"status\":\"failure\",\"message\":\"Check3 is not OK\"}]}",
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockClusterApi.EXPECT().IsReadyForInstallation(gomock.Any()).Return(false, "").Times(1)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))

			aci = getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Message).To(Equal(hiveext.ClusterNotReadyMsg))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterRequirementsMetCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterValidatedCondition).Reason).To(Equal(hiveext.ClusterValidationsFailingReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterValidatedCondition).Message).To(Equal(hiveext.ClusterValidationsFailingMsg + " Check1 is not OK,Check3 is not OK"))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterValidatedCondition).Status).To(Equal(corev1.ConditionFalse))
		})

		It("failed getting cluster", func() {
			expectedErr := "some internal error"
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).
				Return(nil, errors.Errorf(expectedErr))

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))
			aci = getTestClusterInstall()
			expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, expectedErr)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
		})

		It("update internal error", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:                 &sId,
					Name:               "different-cluster-name",
					OpenshiftVersion:   "4.8",
					ClusterNetworkCidr: "11.129.0.0/14",
					NetworkType:        swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:             swag.String(models.ClusterStatusPendingForInput),
				},
				PullSecret: "different-pull-secret",
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			errString := "update internal error"
			mockInstallerInternal.EXPECT().UpdateClusterNonInteractive(gomock.Any(), gomock.Any()).
				Return(nil, errors.Errorf(errString))
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: defaultRequeueAfterOnError}))

			aci = getTestClusterInstall()
			expectedState := fmt.Sprintf("%s %s", hiveext.ClusterBackendErrorMsg, errString)
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterBackendErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Status).To(Equal(corev1.ConditionFalse))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(Equal(expectedState))
		})

		It("add install config overrides annotation", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:                       &sId,
					Name:                     clusterName,
					OpenshiftVersion:         "4.8",
					ClusterNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR,
					ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
					NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:                   swag.String(models.ClusterStatusInsufficient),
					ServiceNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ServiceNetwork[0],
					IngressVip:               defaultAgentClusterInstallSpec.IngressVIP,
					APIVip:                   defaultAgentClusterInstallSpec.APIVIP,
					BaseDNSDomain:            defaultClusterSpec.BaseDomain,
					SSHPublicKey:             defaultAgentClusterInstallSpec.SSHPublicKey,
					Hyperthreading:           models.ClusterHyperthreadingAll,
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			installConfigOverrides := `{"controlPlane": {"hyperthreading": "Disabled"}}`
			updateReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:                     &sId,
					Status:                 swag.String(models.ClusterStatusInsufficient),
					InstallConfigOverrides: installConfigOverrides,
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().UpdateClusterInstallConfigInternal(gomock.Any(), gomock.Any()).
				Do(func(ctx context.Context, param installer.UpdateClusterInstallConfigParams) {
					Expect(param.ClusterID).To(Equal(sId))
					Expect(param.InstallConfigParams).To(Equal(installConfigOverrides))
				}).Return(updateReply, nil)
			// Add annotation
			aci.ObjectMeta.SetAnnotations(map[string]string{InstallConfigOverrides: installConfigOverrides})
			Expect(c.Update(ctx, aci)).Should(BeNil())
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("Remove existing install config overrides annotation", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:                       &sId,
					Name:                     clusterName,
					OpenshiftVersion:         "4.8",
					ClusterNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR,
					ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
					NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:                   swag.String(models.ClusterStatusInsufficient),
					ServiceNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ServiceNetwork[0],
					IngressVip:               defaultAgentClusterInstallSpec.IngressVIP,
					APIVip:                   defaultAgentClusterInstallSpec.APIVIP,
					BaseDNSDomain:            defaultClusterSpec.BaseDomain,
					SSHPublicKey:             defaultAgentClusterInstallSpec.SSHPublicKey,
					Hyperthreading:           models.ClusterHyperthreadingAll,
					InstallConfigOverrides:   `{"controlPlane": {"hyperthreading": "Disabled"}}`,
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			updateReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:                     &sId,
					Status:                 swag.String(models.ClusterStatusInsufficient),
					InstallConfigOverrides: "",
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().UpdateClusterInstallConfigInternal(gomock.Any(), gomock.Any()).
				Do(func(ctx context.Context, param installer.UpdateClusterInstallConfigParams) {
					Expect(param.ClusterID).To(Equal(sId))
					Expect(param.InstallConfigParams).To(Equal(""))
				}).Return(updateReply, nil)
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("Update install config overrides annotation", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:                       &sId,
					Name:                     clusterName,
					OpenshiftVersion:         "4.8",
					ClusterNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR,
					ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
					NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:                   swag.String(models.ClusterStatusInsufficient),
					ServiceNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ServiceNetwork[0],
					IngressVip:               defaultAgentClusterInstallSpec.IngressVIP,
					APIVip:                   defaultAgentClusterInstallSpec.APIVIP,
					BaseDNSDomain:            defaultClusterSpec.BaseDomain,
					SSHPublicKey:             defaultAgentClusterInstallSpec.SSHPublicKey,
					Hyperthreading:           models.ClusterHyperthreadingAll,
					InstallConfigOverrides:   `{"controlPlane": {"hyperthreading": "Disabled"}}`,
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			installConfigOverrides := `{"controlPlane": {"hyperthreading": "Enabled"}}`
			updateReply := &common.Cluster{
				Cluster: models.Cluster{
					ID:                     &sId,
					Status:                 swag.String(models.ClusterStatusInsufficient),
					InstallConfigOverrides: installConfigOverrides,
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().UpdateClusterInstallConfigInternal(gomock.Any(), gomock.Any()).
				Do(func(ctx context.Context, param installer.UpdateClusterInstallConfigParams) {
					Expect(param.ClusterID).To(Equal(sId))
					Expect(param.InstallConfigParams).To(Equal(installConfigOverrides))
				}).Return(updateReply, nil)
			// Add annotation
			aci.ObjectMeta.SetAnnotations(map[string]string{InstallConfigOverrides: installConfigOverrides})
			Expect(c.Update(ctx, aci)).Should(BeNil())
			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("invalid install config overrides annotation", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:                       &sId,
					Name:                     clusterName,
					OpenshiftVersion:         "4.8",
					ClusterNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR,
					ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
					NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:                   swag.String(models.ClusterStatusInsufficient),
					ServiceNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ServiceNetwork[0],
					IngressVip:               defaultAgentClusterInstallSpec.IngressVIP,
					APIVip:                   defaultAgentClusterInstallSpec.APIVIP,
					BaseDNSDomain:            defaultClusterSpec.BaseDomain,
					SSHPublicKey:             defaultAgentClusterInstallSpec.SSHPublicKey,
					Hyperthreading:           models.ClusterHyperthreadingAll,
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			installConfigOverrides := `{{{"controlPlane": ""`
			mockInstallerInternal.EXPECT().UpdateClusterInstallConfigInternal(gomock.Any(), gomock.Any()).
				Do(func(ctx context.Context, param installer.UpdateClusterInstallConfigParams) {
					Expect(param.ClusterID).To(Equal(sId))
					Expect(param.InstallConfigParams).To(Equal(installConfigOverrides))
				}).Return(nil, common.NewApiError(http.StatusBadRequest,
				errors.New("invalid character '{' looking for beginning of object key string")))

			// Add annotation
			aci.ObjectMeta.SetAnnotations(map[string]string{InstallConfigOverrides: installConfigOverrides})
			Expect(c.Update(ctx, aci)).Should(BeNil())
			request := newClusterDeploymentRequest(cluster)
			_, err := cr.Reconcile(ctx, request)
			Expect(err).ShouldNot(HaveOccurred())
			aci := getTestClusterInstall()
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Reason).To(Equal(hiveext.ClusterInputErrorReason))
			Expect(FindStatusCondition(aci.Status.Conditions, hiveext.ClusterSpecSyncedCondition).Message).To(ContainSubstring(
				"Failed to parse 'agent-install.openshift.io/install-config-overrides' annotation"))
		})
	})

	Context("cluster update not needed", func() {
		var (
			sId strfmt.UUID
		)

		BeforeEach(func() {
			id := uuid.New()
			sId = strfmt.UUID(id.String())
		})

		It("SSHPublicKey in ClusterDeployment has spaces in suffix", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:                       &sId,
					Name:                     clusterName,
					OpenshiftVersion:         "4.8",
					ClusterNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR,
					ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
					NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:                   swag.String(models.ClusterStatusInsufficient),
					ServiceNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ServiceNetwork[0],
					IngressVip:               defaultAgentClusterInstallSpec.IngressVIP,
					APIVip:                   defaultAgentClusterInstallSpec.APIVIP,
					BaseDNSDomain:            defaultClusterSpec.BaseDomain,
					SSHPublicKey:             defaultAgentClusterInstallSpec.SSHPublicKey,
					Hyperthreading:           models.ClusterHyperthreadingAll,
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			pullSecret := getDefaultTestPullSecret("pull-secret", testNamespace)
			Expect(c.Create(ctx, pullSecret)).To(BeNil())

			cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
			Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

			aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
			sshPublicKeySuffixSpace := fmt.Sprintf("%s ", defaultAgentClusterInstallSpec.SSHPublicKey)
			aci.Spec.SSHPublicKey = sshPublicKeySuffixSpace
			Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
		})

		It("APIVIP and Ingress VIP set in the backend in case of SNO cluster", func() {
			hostIP := "1.2.3.4"
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:                       &sId,
					Name:                     clusterName,
					OpenshiftVersion:         "4.8",
					ClusterNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR,
					ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
					NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:                   swag.String(models.ClusterStatusInstalling),
					ServiceNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ServiceNetwork[0],
					IngressVip:               hostIP,
					APIVip:                   hostIP,
					BaseDNSDomain:            defaultClusterSpec.BaseDomain,
					SSHPublicKey:             defaultAgentClusterInstallSpec.SSHPublicKey,
					Hyperthreading:           models.ClusterHyperthreadingAll,
					HighAvailabilityMode:     swag.String(models.ClusterHighAvailabilityModeNone),
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockInstallerInternal.EXPECT().DownloadClusterFilesInternal(gomock.Any(), gomock.Any()).Return(ioutil.NopCloser(strings.NewReader("kubeconfig")), int64(len("kubeconfig")), nil).Times(1)

			pullSecret := getDefaultTestPullSecret("pull-secret", testNamespace)
			Expect(c.Create(ctx, pullSecret)).To(BeNil())

			cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
			Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

			aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
			Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
			Expect(aci.Spec.APIVIP).NotTo(Equal(hostIP))
			Expect(aci.Spec.IngressVIP).NotTo(Equal(hostIP))
		})

		It("DHCP is enabled", func() {
			backEndCluster := &common.Cluster{
				Cluster: models.Cluster{
					ID:                       &sId,
					Name:                     clusterName,
					OpenshiftVersion:         "4.8",
					ClusterNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].CIDR,
					ClusterNetworkHostPrefix: int64(defaultAgentClusterInstallSpec.Networking.ClusterNetwork[0].HostPrefix),
					NetworkType:              swag.String(models.ClusterNetworkTypeOpenShiftSDN),
					Status:                   swag.String(models.ClusterStatusInstalling),
					ServiceNetworkCidr:       defaultAgentClusterInstallSpec.Networking.ServiceNetwork[0],
					IngressVip:               defaultAgentClusterInstallSpec.IngressVIP,
					APIVip:                   defaultAgentClusterInstallSpec.APIVIP,
					BaseDNSDomain:            defaultClusterSpec.BaseDomain,
					SSHPublicKey:             defaultAgentClusterInstallSpec.SSHPublicKey,
					Hyperthreading:           models.ClusterHyperthreadingAll,
					VipDhcpAllocation:        swag.Bool(true),
				},
				PullSecret: testPullSecretVal,
			}
			mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
			mockInstallerInternal.EXPECT().DownloadClusterFilesInternal(gomock.Any(), gomock.Any()).Return(ioutil.NopCloser(strings.NewReader("kubeconfig")), int64(len("kubeconfig")), nil).Times(1)

			pullSecret := getDefaultTestPullSecret("pull-secret", testNamespace)
			Expect(c.Create(ctx, pullSecret)).To(BeNil())

			cluster := newClusterDeployment(clusterName, testNamespace, defaultClusterSpec)
			Expect(c.Create(ctx, cluster)).ShouldNot(HaveOccurred())

			aci := newAgentClusterInstall(agentClusterInstallName, testNamespace, defaultAgentClusterInstallSpec, cluster)
			Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())

			request := newClusterDeploymentRequest(cluster)
			result, err := cr.Reconcile(ctx, request)
			Expect(err).To(BeNil())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})
})

var _ = Describe("TestConditions", func() {
	var (
		c                      client.Client
		cr                     *ClusterDeploymentsReconciler
		ctx                    = context.Background()
		mockCtrl               *gomock.Controller
		backEndCluster         *common.Cluster
		clusterRequest         ctrl.Request
		clusterKey             types.NamespacedName
		agentClusterInstallKey types.NamespacedName
	)

	BeforeEach(func() {
		c = fakeclient.NewClientBuilder().WithScheme(scheme.Scheme).Build()
		mockCtrl = gomock.NewController(GinkgoT())
		mockInstallerInternal := bminventory.NewMockInstallerInternals(mockCtrl)
		mockClusterApi := cluster.NewMockAPI(mockCtrl)
		cr = &ClusterDeploymentsReconciler{
			Client:     c,
			APIReader:  c,
			Scheme:     scheme.Scheme,
			Log:        common.GetTestLog(),
			Installer:  mockInstallerInternal,
			ClusterApi: mockClusterApi,
		}
		backEndCluster = &common.Cluster{}
		clusterKey = types.NamespacedName{
			Namespace: testNamespace,
			Name:      "clusterDeployment",
		}
		agentClusterInstallKey = types.NamespacedName{
			Namespace: testNamespace,
			Name:      "agentClusterInstall",
		}
		clusterDeployment := newClusterDeployment(clusterKey.Name, clusterKey.Namespace, getDefaultClusterDeploymentSpec("clusterDeployment-test", agentClusterInstallKey.Name, "pull-secret"))
		Expect(c.Create(ctx, clusterDeployment)).To(BeNil())
		aci := newAgentClusterInstall(agentClusterInstallKey.Name, agentClusterInstallKey.Namespace, getDefaultAgentClusterInstallSpec(clusterKey.Name), clusterDeployment)
		aci.Spec.ProvisionRequirements.WorkerAgents = 0
		aci.Spec.ProvisionRequirements.ControlPlaneAgents = 0
		Expect(c.Create(ctx, aci)).ShouldNot(HaveOccurred())
		clusterRequest = newClusterDeploymentRequest(clusterDeployment)
		mockInstallerInternal.EXPECT().GetClusterByKubeKey(gomock.Any()).Return(backEndCluster, nil)
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	tests := []struct {
		name           string
		clusterStatus  string
		statusInfo     string
		validationInfo string
		conditions     []hivev1.ClusterInstallCondition
	}{
		{
			name:           "Unsufficient",
			clusterStatus:  models.ClusterStatusInsufficient,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking1\",\"status\":\"failure\",\"message\":\"Check1 is not OK\"},{\"id\":\"checking2\",\"status\":\"success\",\"message\":\"Check2 is OK\"},{\"id\":\"checking3\",\"status\":\"failure\",\"message\":\"Check3 is not OK\"}]}",
			conditions: []hivev1.ClusterInstallCondition{
				{
					Type:    hiveext.ClusterRequirementsMetCondition,
					Message: hiveext.ClusterNotReadyMsg,
					Reason:  hiveext.ClusterNotReadyReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterCompletedCondition,
					Message: hiveext.ClusterInstallationNotStartedMsg,
					Reason:  hiveext.ClusterInstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterValidatedCondition,
					Message: hiveext.ClusterValidationsFailingMsg + " Check1 is not OK,Check3 is not OK",
					Reason:  hiveext.ClusterValidationsFailingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterFailedCondition,
					Message: hiveext.ClusterNotFailedMsg,
					Reason:  hiveext.ClusterNotFailedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterStoppedCondition,
					Message: hiveext.ClusterNotStoppedMsg,
					Reason:  hiveext.ClusterNotStoppedReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "PendingForInput",
			clusterStatus:  models.ClusterStatusPendingForInput,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking1\",\"status\":\"failure\",\"message\":\"Check1 is not OK\"},{\"id\":\"checking2\",\"status\":\"success\",\"message\":\"Check2 is OK\"},{\"id\":\"checking3\",\"status\":\"failure\",\"message\":\"Check3 is not OK\"},{\"id\":\"checking4\",\"status\":\"pending\",\"message\":\"Check4 is pending\"}]}",
			conditions: []hivev1.ClusterInstallCondition{
				{
					Type:    hiveext.ClusterRequirementsMetCondition,
					Message: hiveext.ClusterNotReadyMsg,
					Reason:  hiveext.ClusterNotReadyReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterCompletedCondition,
					Message: hiveext.ClusterInstallationNotStartedMsg,
					Reason:  hiveext.ClusterInstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterValidatedCondition,
					Message: hiveext.ClusterValidationsUserPendingMsg + " Check1 is not OK,Check3 is not OK,Check4 is pending",
					Reason:  hiveext.ClusterValidationsUserPendingReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterFailedCondition,
					Message: hiveext.ClusterNotFailedMsg,
					Reason:  hiveext.ClusterNotFailedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterStoppedCondition,
					Message: hiveext.ClusterNotStoppedMsg,
					Reason:  hiveext.ClusterNotStoppedReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "AddingHosts",
			clusterStatus:  models.ClusterStatusAddingHosts,
			statusInfo:     "Done",
			validationInfo: "",
			conditions: []hivev1.ClusterInstallCondition{
				{
					Type:    hiveext.ClusterRequirementsMetCondition,
					Message: hiveext.ClusterAlreadyInstallingMsg,
					Reason:  hiveext.ClusterAlreadyInstallingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterCompletedCondition,
					Message: hiveext.ClusterInstalledMsg + " Done",
					Reason:  hiveext.ClusterInstalledReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterValidatedCondition,
					Message: hiveext.ClusterValidationsOKMsg,
					Reason:  hiveext.ClusterValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterFailedCondition,
					Message: hiveext.ClusterNotFailedMsg,
					Reason:  hiveext.ClusterNotFailedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterStoppedCondition,
					Message: hiveext.ClusterNotStoppedMsg,
					Reason:  hiveext.ClusterNotStoppedReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "Installed",
			clusterStatus:  models.ClusterStatusInstalled,
			statusInfo:     "Done",
			validationInfo: "{\"some-check\":[{\"id\":\"checking2\",\"status\":\"success\",\"message\":\"Check2 is OK\"}]}",
			conditions: []hivev1.ClusterInstallCondition{
				{
					Type:    hiveext.ClusterRequirementsMetCondition,
					Message: hiveext.ClusterInstallationStoppedMsg,
					Reason:  hiveext.ClusterInstallationStoppedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterCompletedCondition,
					Message: hiveext.ClusterInstalledMsg + " Done",
					Reason:  hiveext.ClusterInstalledReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterValidatedCondition,
					Message: hiveext.ClusterValidationsOKMsg,
					Reason:  hiveext.ClusterValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterFailedCondition,
					Message: hiveext.ClusterNotFailedMsg,
					Reason:  hiveext.ClusterNotFailedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterStoppedCondition,
					Message: hiveext.ClusterStoppedCompletedMsg,
					Reason:  hiveext.ClusterStoppedCompletedReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
		{
			name:           "Installing",
			clusterStatus:  models.ClusterStatusInstalling,
			statusInfo:     "Phase 1",
			validationInfo: "{\"some-check\":[{\"id\":\"checking2\",\"status\":\"success\",\"message\":\"Check2 is OK\"}]}",
			conditions: []hivev1.ClusterInstallCondition{
				{
					Type:    hiveext.ClusterRequirementsMetCondition,
					Message: hiveext.ClusterAlreadyInstallingMsg,
					Reason:  hiveext.ClusterAlreadyInstallingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterCompletedCondition,
					Message: hiveext.ClusterInstallationInProgressMsg + " Phase 1",
					Reason:  hiveext.ClusterInstallationInProgressReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterValidatedCondition,
					Message: hiveext.ClusterValidationsOKMsg,
					Reason:  hiveext.ClusterValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterFailedCondition,
					Message: hiveext.ClusterNotFailedMsg,
					Reason:  hiveext.ClusterNotFailedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterStoppedCondition,
					Message: hiveext.ClusterNotStoppedMsg,
					Reason:  hiveext.ClusterNotStoppedReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "Ready",
			clusterStatus:  models.ClusterStatusReady,
			statusInfo:     "",
			validationInfo: "{\"some-check\":[{\"id\":\"checking2\",\"status\":\"success\",\"message\":\"Check2 is OK\"}]}",
			conditions: []hivev1.ClusterInstallCondition{
				{
					Type:    hiveext.ClusterRequirementsMetCondition,
					Message: hiveext.ClusterReadyMsg,
					Reason:  hiveext.ClusterReadyReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterCompletedCondition,
					Message: hiveext.ClusterInstallationNotStartedMsg,
					Reason:  hiveext.ClusterInstallationNotStartedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterValidatedCondition,
					Message: hiveext.ClusterValidationsOKMsg,
					Reason:  hiveext.ClusterValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterFailedCondition,
					Message: hiveext.ClusterNotFailedMsg,
					Reason:  hiveext.ClusterNotFailedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterStoppedCondition,
					Message: hiveext.ClusterNotStoppedMsg,
					Reason:  hiveext.ClusterNotStoppedReason,
					Status:  corev1.ConditionFalse,
				},
			},
		},
		{
			name:           "Error",
			clusterStatus:  models.ClusterStatusError,
			statusInfo:     "failed due to some error",
			validationInfo: "{\"some-check\":[{\"id\":\"checking2\",\"status\":\"success\",\"message\":\"Check2 is OK\"}]}",
			conditions: []hivev1.ClusterInstallCondition{
				{
					Type:    hiveext.ClusterRequirementsMetCondition,
					Message: hiveext.ClusterInstallationStoppedMsg,
					Reason:  hiveext.ClusterInstallationStoppedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterCompletedCondition,
					Message: hiveext.ClusterInstallationFailedMsg + " failed due to some error",
					Reason:  hiveext.ClusterInstallationFailedReason,
					Status:  corev1.ConditionFalse,
				},
				{
					Type:    hiveext.ClusterValidatedCondition,
					Message: hiveext.ClusterValidationsOKMsg,
					Reason:  hiveext.ClusterValidationsPassingReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterFailedCondition,
					Message: hiveext.ClusterFailedMsg + " failed due to some error",
					Reason:  hiveext.ClusterFailedReason,
					Status:  corev1.ConditionTrue,
				},
				{
					Type:    hiveext.ClusterStoppedCondition,
					Message: hiveext.ClusterStoppedFailedMsg,
					Reason:  hiveext.ClusterStoppedFailedReason,
					Status:  corev1.ConditionTrue,
				},
			},
		},
	}

	for i := range tests {
		t := tests[i]
		It(t.name, func() {
			backEndCluster.Status = swag.String(t.clusterStatus)
			backEndCluster.StatusInfo = swag.String(t.statusInfo)
			backEndCluster.ValidationsInfo = t.validationInfo
			cid := strfmt.UUID(uuid.New().String())
			backEndCluster.ID = &cid
			_, err := cr.Reconcile(ctx, clusterRequest)
			Expect(err).To(BeNil())
			cluster := &hivev1.ClusterDeployment{}
			Expect(c.Get(ctx, clusterKey, cluster)).To(BeNil())
			clusterInstall := &hiveext.AgentClusterInstall{}
			Expect(c.Get(ctx, agentClusterInstallKey, clusterInstall)).To(BeNil())
			for _, cond := range t.conditions {
				Expect(FindStatusCondition(clusterInstall.Status.Conditions, cond.Type).Message).To(Equal(cond.Message))
				Expect(FindStatusCondition(clusterInstall.Status.Conditions, cond.Type).Reason).To(Equal(cond.Reason))
				Expect(FindStatusCondition(clusterInstall.Status.Conditions, cond.Type).Status).To(Equal(cond.Status))
			}
			Expect(clusterInstall.Status.DebugInfo.State).To(Equal(t.clusterStatus))
			Expect(clusterInstall.Status.DebugInfo.StateInfo).To(Equal(t.statusInfo))
		})
	}
})

var _ = Describe("selectClusterNetworkType", func() {
	tests := []struct {
		clusterServiceNetworkCidr string
		paramServiceNetworkCidr   string
		resultNetworkType         string
	}{
		{
			clusterServiceNetworkCidr: "1.2.8.0/23",
			paramServiceNetworkCidr:   "",
			resultNetworkType:         models.ClusterNetworkTypeOpenShiftSDN,
		},
		{
			clusterServiceNetworkCidr: "1002:db8::/119",
			paramServiceNetworkCidr:   "",
			resultNetworkType:         models.ClusterNetworkTypeOVNKubernetes,
		},
		{
			clusterServiceNetworkCidr: "1.2.8.0/23",
			paramServiceNetworkCidr:   "1.2.8.0/23",
			resultNetworkType:         models.ClusterNetworkTypeOpenShiftSDN,
		},
		{
			clusterServiceNetworkCidr: "1002:db8::/119",
			paramServiceNetworkCidr:   "1.2.8.0/23",
			resultNetworkType:         models.ClusterNetworkTypeOpenShiftSDN,
		},
		{
			clusterServiceNetworkCidr: "1002:db8::/119",
			paramServiceNetworkCidr:   "1.2.8.0/23",
			resultNetworkType:         models.ClusterNetworkTypeOpenShiftSDN,
		},
		{
			clusterServiceNetworkCidr: "1002:db8::/119",
			paramServiceNetworkCidr:   "1003:db8::/119",
			resultNetworkType:         models.ClusterNetworkTypeOVNKubernetes,
		},
	}
	for i := range tests {
		t := tests[i]
		It("getNetworkType", func() {
			ClusterUpdateParams := &models.ClusterUpdateParams{}
			if t.paramServiceNetworkCidr != "" {
				ClusterUpdateParams = &models.ClusterUpdateParams{ServiceNetworkCidr: &t.paramServiceNetworkCidr}
			}

			cluster := &common.Cluster{Cluster: models.Cluster{
				ServiceNetworkCidr: t.clusterServiceNetworkCidr,
				ClusterNetworkCidr: "10.56.20.0/24",
				MachineNetworkCidr: "1.2.3.0/24",
			}}
			networkType := selectClusterNetworkType(ClusterUpdateParams, cluster)
			Expect(networkType).To(Equal(t.resultNetworkType))
		})
	}
})
