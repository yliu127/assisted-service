// Code generated by MockGen. DO NOT EDIT.
// Source: pullsecret_auth.go

// Package ocm is a generated GoMock package.
package ocm

import (
	context "context"
	reflect "reflect"

	gomock "github.com/golang/mock/gomock"
)

// MockOCMAuthentication is a mock of OCMAuthentication interface
type MockOCMAuthentication struct {
	ctrl     *gomock.Controller
	recorder *MockOCMAuthenticationMockRecorder
}

// MockOCMAuthenticationMockRecorder is the mock recorder for MockOCMAuthentication
type MockOCMAuthenticationMockRecorder struct {
	mock *MockOCMAuthentication
}

// NewMockOCMAuthentication creates a new mock instance
func NewMockOCMAuthentication(ctrl *gomock.Controller) *MockOCMAuthentication {
	mock := &MockOCMAuthentication{ctrl: ctrl}
	mock.recorder = &MockOCMAuthenticationMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use
func (m *MockOCMAuthentication) EXPECT() *MockOCMAuthenticationMockRecorder {
	return m.recorder
}

// AuthenticatePullSecret mocks base method
func (m *MockOCMAuthentication) AuthenticatePullSecret(ctx context.Context, pullSecret string) (*AuthPayload, error) {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "AuthenticatePullSecret", ctx, pullSecret)
	ret0, _ := ret[0].(*AuthPayload)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

// AuthenticatePullSecret indicates an expected call of AuthenticatePullSecret
func (mr *MockOCMAuthenticationMockRecorder) AuthenticatePullSecret(ctx, pullSecret interface{}) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "AuthenticatePullSecret", reflect.TypeOf((*MockOCMAuthentication)(nil).AuthenticatePullSecret), ctx, pullSecret)
}
