/*


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

package v1beta1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var maasclusterlog = logf.Log.WithName("maascluster-resource")

func (r *MaasCluster) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, r).
		WithCustomDefaulter(r).
		WithCustomValidator(r).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-infrastructure-cluster-x-k8s-io-v1beta1-maascluster,mutating=true,failurePolicy=fail,groups=infrastructure.cluster.x-k8s.io,resources=maasclusters,verbs=create;update,versions=v1beta1,name=mmaascluster.kb.io,sideEffects=None,admissionReviewVersions=v1beta1;v1
//+kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1beta1-maascluster,mutating=false,failurePolicy=fail,groups=infrastructure.cluster.x-k8s.io,resources=maasclusters,versions=v1beta1,name=vmaascluster.kb.io,sideEffects=None,admissionReviewVersions=v1beta1;v1

var (
	_ webhook.CustomDefaulter = &MaasCluster{}
	_ webhook.CustomValidator = &MaasCluster{}
)

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *MaasCluster) Default(ctx context.Context, obj runtime.Object) error {
	r, ok := obj.(*MaasCluster)
	if !ok {
		return fmt.Errorf("expected *MaasCluster, got %T", obj)
	}
	maasclusterlog.Info("default", "name", r.Name)
	return nil
}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *MaasCluster) ValidateCreate(ctx context.Context, obj runtime.Object) (warnings admission.Warnings, err error) {
	r, ok := obj.(*MaasCluster)
	if !ok {
		return nil, fmt.Errorf("expected *MaasCluster, got %T", obj)
	}
	maasclusterlog.Info("validate create", "name", r.Name)
	return nil, nil
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *MaasCluster) ValidateUpdate(ctx context.Context, old runtime.Object, new runtime.Object) (warnings admission.Warnings, err error) {
	r, ok := new.(*MaasCluster)
	if !ok {
		return nil, fmt.Errorf("expected *MaasCluster, got %T", new)
	}
	maasclusterlog.Info("validate update", "name", r.Name)
	oldC, ok := old.(*MaasCluster)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a MaasCluster but got a %T", old))
	}

	if r.Spec.DNSDomain != oldC.Spec.DNSDomain {
		return nil, apierrors.NewBadRequest("changing cluster DNS Domain not allowed")
	}
	return nil, nil
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *MaasCluster) ValidateDelete(ctx context.Context, obj runtime.Object) (warnings admission.Warnings, err error) {
	maasclusterlog.Info("validate delete", "name", r.Name)
	return nil, nil
}
