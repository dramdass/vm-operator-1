// Copyright (c) 2018-2020 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package vsphere

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"

	ctrlruntime "sigs.k8s.io/controller-runtime/pkg/client"
)

type SessionManager struct {
	client    *Client
	k8sClient ctrlruntime.Client
	scheme    *runtime.Scheme

	// sessions contains the map of sessions for each namespace.
	mutex    sync.Mutex
	sessions map[string]*Session
}

func NewSessionManager(k8sClient ctrlruntime.Client, scheme *runtime.Scheme) SessionManager {
	return SessionManager{
		k8sClient: k8sClient,
		scheme:    scheme,
		sessions:  make(map[string]*Session),
	}
}

func (sm *SessionManager) getClient(context context.Context, config *VSphereVmProviderConfig) (*Client, error) {
	sm.mutex.Lock()
	client := sm.client
	sm.mutex.Unlock()
	if client != nil {
		return client, nil
	}

	client, err := NewClient(context, config)
	if err != nil {
		return nil, err
	}

	sm.mutex.Lock()
	if sm.client == nil {
		sm.client = client
		client = nil
	}
	sm.mutex.Unlock()
	if client != nil {
		client.Logout(context)
	}

	return sm.client, nil
}

func (sm *SessionManager) createSession(ctx context.Context, namespace string) (*Session, error) {
	config, err := GetProviderConfigFromConfigMap(sm.k8sClient, namespace)
	if err != nil {
		return nil, err
	}

	log.V(4).Info("Create session", "namespace", namespace, "config", config)

	client, err := sm.getClient(ctx, config)
	if err != nil {
		return nil, err
	}

	ses, err := NewSessionAndConfigure(ctx, client, config, sm.k8sClient, sm.scheme)
	if err != nil {
		return nil, err
	}

	return ses, nil
}

func (sm *SessionManager) GetSession(ctx context.Context, namespace string) (*Session, error) {
	sm.mutex.Lock()
	ses, ok := sm.sessions[namespace]
	sm.mutex.Unlock()

	if ok {
		return ses, nil
	}

	ses, err := sm.createSession(ctx, namespace)
	if err != nil {
		return nil, err
	}

	sm.mutex.Lock()
	sm.sessions[namespace] = ses
	sm.mutex.Unlock()

	return ses, nil
}

func (sm *SessionManager) ComputeClusterCpuMinFrequency(ctx context.Context) error {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	var minFreq uint64
	var err error
	for _, s := range sm.sessions {
		minFreq, err = s.computeCPUInfo(ctx)
		break
	}
	if err != nil {
		return err
	}

	for _, s := range sm.sessions {
		s.SetCpuMinMHzInCluster(minFreq)
	}

	return nil
}

func (sm *SessionManager) UpdateVcPNID(ctx context.Context, vcPNID, vcPort string) error {
	config, err := GetProviderConfigFromConfigMap(sm.k8sClient, "")
	if err != nil {
		return err
	}

	if config.VcPNID == vcPNID && config.VcPort == vcPort {
		return nil
	}

	if err = PatchVcURLInConfigMap(sm.k8sClient, vcPNID, vcPort); err != nil {
		return err
	}

	sm.clearSessionsAndClient(ctx)

	return nil
}

func (sm *SessionManager) clearSessionsAndClient(ctx context.Context) {
	sm.mutex.Lock()
	defer sm.mutex.Unlock()

	for ns := range sm.sessions {
		delete(sm.sessions, ns)
	}

	if sm.client != nil {
		sm.client.Logout(ctx)
		sm.client = nil
	}
}
