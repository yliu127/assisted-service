// Code generated by MockGen. DO NOT EDIT.
// Source: github.com/openshift/assisted-service/internal/bminventory (interfaces: CRDUtils)

// Package bminventory is a generated GoMock package.
package bminventory

import (
	context "context"
	gomock "github.com/golang/mock/gomock"
	common "github.com/openshift/assisted-service/internal/common"
	logrus "github.com/sirupsen/logrus"
	reflect "reflect"
)

// MockCRDUtils is a mock of CRDUtils interface
type MockCRDUtils struct {
	ctrl     *gomock.Controller
	recorder *MockCRDUtilsMockRecorder
}

// MockCRDUtilsMockRecorder is the mock recorder for MockCRDUtils
type MockCRDUtilsMockRecorder struct {
	mock *MockCRDUtils
}

// NewMockCRDUtils creates a new mock instance
func NewMockCRDUtils(ctrl *gomock.Controller) *MockCRDUtils {
	mock := &MockCRDUtils{ctrl: ctrl}
	mock.recorder = &MockCRDUtilsMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockCRDUtils) EXPECT() *MockCRDUtilsMockRecorder {
	return m.recorder
}

// CreateAgentCR mocks base method
func (m *MockCRDUtils) CreateAgentCR(arg0 context.Context, arg1 logrus.FieldLogger, arg2 string, arg3 *common.InfraEnv, arg4 *common.Cluster) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "CreateAgentCR", arg0, arg1, arg2, arg3, arg4)
	ret0, _ := ret[0].(error)
	return ret0
}

// CreateAgentCR indicates an expected call of CreateAgentCR
func (mr *MockCRDUtilsMockRecorder) CreateAgentCR(arg0, arg1, arg2, arg3, arg4 interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "CreateAgentCR", reflect.TypeOf((*MockCRDUtils)(nil).CreateAgentCR), arg0, arg1, arg2, arg3, arg4)
}
