// Copyright 2018 The rss-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cluster

import (
	"errors"
	"fmt"

	api "github.com/beekhof/rss-operator/pkg/apis/clusterlabs/v1alpha1"
	"github.com/beekhof/rss-operator/pkg/util"

	"github.com/sirupsen/logrus"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrLostQuorum indicates that the etcd cluster lost its quorum.
var (
	ErrLostQuorum = errors.New("lost quorum")
)

// reconcile reconciles cluster current state to desired state specified by spec.
// - it tries to reconcile the cluster to desired size.
// - if the cluster needs for upgrade, it tries to upgrade old member one by one.
func (c *Cluster) recover(peers util.MemberSet) []error {
	var errors []error
	c.logger.Debug("Start recovery")
	defer c.logger.Debug("Finish recovery")
	for _, m := range peers {

		// TODO: Make the threshold configurable
		// ' > 1' means that we tried at least a start and a stop
		if m.AppFailed && m.Failures > 1 {
			errors = append(errors, fmt.Errorf("%v deletion after %v failures", m.Name, m.Failures))
			errors = appendNonNil(errors, c.deleteMember(m))
			c.tagAppMember(m, false)

		} else if !m.Online {
			c.logger.Debugf("reconcile: Skipping offline pod %v", m.Name)
			continue

		} else if m.AppFailed {
			c.logger.Warnf("reconcile: Cleaning up pod %v", m.Name)
			if err := c.stopAppMember(m); err != nil {
				errors = append(errors, fmt.Errorf("%v deletion after stop failure: %v", m.Name, err))
				errors = appendNonNil(errors, c.deleteMember(m))
			}
		}
	}
	return errors
}

func (c *Cluster) reconcile(pods []*v1.Pod) []error {
	var errors []error
	c.logger.Debug("Start reconciling")
	defer c.logger.Debug("Finish reconciling")

	sp := c.rss.Spec
	running := c.podsToMemberSet(pods, c.isSecureClient())

	// On controller restore, we could have "members == nil"
	if c.peers == nil {
		c.peers = util.MemberSet{}
	}

	var err error
	last := c.peers

	c.peers, err = c.peers.Reconcile(running, c.rss.Spec.GetNumReplicas())
	errors = appendNonNil(errors, err)

	for _, m := range c.peers {

		needOutput := false
		// TODO: Make the threshold configurable
		// ' > 1' means that we tried at least a start and a stop
		if m.AppFailed || !m.Online {
			// c.tagAppMember(m, false) // In case it failed last time?
			continue
		}

		stdout, stderr, err, rc := c.execute(api.StatusCommandKey, m.Name, true)

		if _, ok := c.rss.Spec.Pod.Commands[api.SecondaryCommandKey]; rc == 0 && !ok {
			// Secondaries are not in use, map to primary
			rc = 8
			err = fmt.Errorf("remapped from 0")
		}

		switch rc {
		case 0:
			if !m.AppRunning {
				c.logger.Infof("reconcile: Detected active applcation on %v", m.Name)
			} else if m.AppPrimary {
				c.logger.Warnf("reconcile: Detected demoted primary on %v", m.Name)
				needOutput = true
			}
			m.AppRunning = true
			m.AppPrimary = false
		case 7:
			if m.AppRunning {
				c.logger.Warnf("reconcile: Detected stopped applcation on %v: %v", m.Name, err)
				errors = appendNonNil(errors, err)
				needOutput = true
			}
			m.AppRunning = false
			m.AppPrimary = false
		case 8:
			if !m.AppRunning {
				c.logger.Infof("reconcile: Detected active primary applcation on %v", m.Name)
			} else if !m.AppPrimary {
				c.logger.Warnf("reconcile: Detected promoted secondary on %v: %v", m.Name, err)
				errors = appendNonNil(errors, err)
				needOutput = true
			}
			m.AppPrimary = true
			m.AppRunning = true
		default:
			c.logger.Errorf("reconcile: Check failed on %v: %v", m.Name, err)
			errors = appendNonNil(errors, err)
			m.AppRunning = true
			m.AppFailed = true
			needOutput = true
		}

		if needOutput {
			c.logger.Errorf("Application check on pod %v failed: %v", m.Name, err)
			util.LogOutput(c.logger.WithField("check", "stdout"), logrus.ErrorLevel, m.Name, stdout)
			util.LogOutput(c.logger.WithField("check", "stderr"), logrus.ErrorLevel, m.Name, stderr)
		} else {
			c.logger.Debugf("Application check on pod %v passed", m.Name)
			util.LogOutput(c.logger.WithField("check", "stderr"), logrus.DebugLevel, m.Name, stderr)
		}

		if m.AppFailed {
			c.tagAppMember(m, false)
		}
	}

	errors = appendAllNonNil(errors, c.recover(c.peers))

	c.logger.Debugf("    running members: %s", running)
	c.logger.Debugf("previous membership: %s", last)
	c.logger.Infof(" current membership: %s", c.peers)

	if c.peers.ActiveMembers() > sp.GetNumReplicas() {
		c.status.SetScalingDownCondition(c.peers.ActiveMembers(), sp.GetNumReplicas())

	} else if c.peers.ActiveMembers() < sp.GetNumReplicas() {
		c.status.SetScalingUpCondition(c.peers.ActiveMembers(), sp.GetNumReplicas())

	} else if len(errors) > 0 {
		c.status.SetRecoveringCondition()

	} else {
		c.status.SetReadyCondition()
	}

	c.status.Replicas = len(c.peers)
	c.updateCRStatus("reconcile")

	return errors
}

func (c *Cluster) podsToMemberSet(pods []*v1.Pod, sc bool) util.MemberSet {
	members := util.MemberSet{}
	for _, pod := range pods {
		m := c.newMember(pod.Name, pod.Namespace)
		m.Online = true
		members.Add(m)
	}
	return members
}

func (c *Cluster) newMember(name string, namespace string) *util.Member {
	if namespace == "" {
		namespace = c.rss.Namespace
	}
	return &util.Member{
		Name:         name,
		Namespace:    namespace,
		SecurePeer:   c.isSecurePeer(),
		SecureClient: c.isSecureClient(),
	}
}

func (c *Cluster) deleteMember(m *util.Member) error {
	err := c.config.KubeCli.CoreV1().Pods(c.rss.Namespace).Delete(m.Name, &metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("reconcile: could not delete pod %v", m.Name, err)
	}
	c.logger.Warnf("reconcile: deleted pod %v", m.Name)
	m.Offline()
	return nil
}
