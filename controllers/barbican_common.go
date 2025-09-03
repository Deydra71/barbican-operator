/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	barbicanv1beta1 "github.com/openstack-k8s-operators/barbican-operator/api/v1beta1"
	topologyv1 "github.com/openstack-k8s-operators/infra-operator/apis/topology/v1beta1"
	"github.com/openstack-k8s-operators/lib-common/modules/common/condition"
	"github.com/openstack-k8s-operators/lib-common/modules/common/env"
	"github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	"github.com/openstack-k8s-operators/lib-common/modules/common/secret"
	"github.com/openstack-k8s-operators/lib-common/modules/common/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type conditionUpdater interface {
	Set(c *condition.Condition)
	MarkTrue(t condition.Type, messageFormat string, messageArgs ...interface{})
}

type topologyHandler interface {
	GetSpecTopologyRef() *topologyv1.TopoRef
	GetLastAppliedTopology() *topologyv1.TopoRef
	SetLastAppliedTopology(t *topologyv1.TopoRef)
}

func ensureTopology(
	ctx context.Context,
	helper *helper.Helper,
	instance topologyHandler,
	finalizer string,
	conditionUpdater conditionUpdater,
	defaultLabelSelector metav1.LabelSelector,
) (*topologyv1.Topology, error) {

	topology, err := topologyv1.EnsureServiceTopology(
		ctx,
		helper,
		instance.GetSpecTopologyRef(),
		instance.GetLastAppliedTopology(),
		finalizer,
		defaultLabelSelector,
	)
	if err != nil {
		conditionUpdater.Set(condition.FalseCondition(
			condition.TopologyReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			condition.TopologyReadyErrorMessage,
			err.Error()))
		return nil, fmt.Errorf("waiting for Topology requirements: %w", err)
	}
	// update the Status with the last retrieved Topology (or set it to nil)
	instance.SetLastAppliedTopology(instance.GetSpecTopologyRef())
	// update the Topology condition only when a Topology is referenced and has
	// been retrieved (err == nil)
	if tr := instance.GetSpecTopologyRef(); tr != nil {
		// update the TopologyRef associated condition
		conditionUpdater.MarkTrue(
			condition.TopologyReadyCondition,
			condition.TopologyReadyMessage,
		)
	}
	return topology, nil
}

// GenerateConfigsGeneric - generates config files
func GenerateConfigsGeneric(
	ctx context.Context, h *helper.Helper,
	instance client.Object,
	envVars *map[string]env.Setter,
	templateParameters map[string]interface{},
	customData map[string]string,
	cmLabels map[string]string,
	scripts bool,
) error {

	cms := []util.Template{
		// Templates where the BarbicanAPI config is stored
		{
			Name:          fmt.Sprintf("%s-config-data", instance.GetName()),
			Namespace:     instance.GetNamespace(),
			Type:          util.TemplateTypeConfig,
			InstanceType:  instance.GetObjectKind().GroupVersionKind().Kind,
			ConfigOptions: templateParameters,
			CustomData:    customData,
			Labels:        cmLabels,
		},
	}
	if scripts {
		cms = append(cms, util.Template{
			Name:          fmt.Sprintf("%s-scripts", instance.GetName()),
			Namespace:     instance.GetNamespace(),
			Type:          util.TemplateTypeScripts,
			InstanceType:  instance.GetObjectKind().GroupVersionKind().Kind,
			ConfigOptions: templateParameters,
			Labels:        cmLabels,
		})
	}
	return secret.EnsureSecrets(ctx, h, instance, cms, envVars)
}

func GenerateSecretStoreTemplateMap(
	enabledSecretStores []barbicanv1beta1.SecretStore,
	globalDefaultSecretStore barbicanv1beta1.SecretStore,
) (map[string]interface{}, error) {
	// Log := r.GetLogger(ctx)
	stores := []string{}
	if len(enabledSecretStores) == 0 {
		stores = []string{"simple_crypto"}
	} else {
		for _, value := range enabledSecretStores {
			stores = append(stores, string(value))
		}
	}

	if len(globalDefaultSecretStore) == 0 {
		globalDefaultSecretStore = "simple_crypto"
	}

	tempMap := map[string]interface{}{
		"EnabledSecretStores":      strings.Join(stores, ","),
		"GlobalDefaultSecretStore": globalDefaultSecretStore,
		"SimpleCryptoEnabled":      slices.Contains(stores, "simple_crypto"),
		"PKCS11CryptoEnabled":      slices.Contains(stores, "pkcs11"),
	}
	return tempMap, nil
}

// AddKeystoneOverridesWatches adds keystone-overrides secret watch to the passed controller builder
func AddKeystoneOverridesWatches(b *builder.Builder) *builder.Builder {
	keystoneOverridesMap := handler.MapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		name := obj.GetName()
		ns := obj.GetNamespace()

		// Only watch for secrets named "keystone-overrides"
		if name != "keystone-overrides" {
			return nil
		}

		// Only handle Secret objects
		if _, isSecret := obj.(*corev1.Secret); !isSecret {
			return nil
		}

		// Reconcile all Barbican instances when keystone-overrides secret changes
		return []reconcile.Request{
			{NamespacedName: types.NamespacedName{Namespace: ns, Name: "barbican"}},
		}
	})

	// Watch the keystone-overrides secret
	b = b.Watches(
		&corev1.Secret{},
		handler.EnqueueRequestsFromMapFunc(keystoneOverridesMap),
		builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
	)

	return b
}
