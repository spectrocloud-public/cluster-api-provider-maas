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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var maasclusterlog = logf.Log.WithName("maascluster-resource")

func (r *MaasCluster) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, r).
		WithDefaulter(r).
		WithValidator(r).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-infrastructure-cluster-x-k8s-io-v1beta1-maascluster,mutating=true,failurePolicy=fail,groups=infrastructure.cluster.x-k8s.io,resources=maasclusters,verbs=create;update,versions=v1beta1,name=mmaascluster.kb.io,sideEffects=None,admissionReviewVersions=v1beta1;v1
//+kubebuilder:webhook:verbs=create;update,path=/validate-infrastructure-cluster-x-k8s-io-v1beta1-maascluster,mutating=false,failurePolicy=fail,groups=infrastructure.cluster.x-k8s.io,resources=maasclusters,versions=v1beta1,name=vmaascluster.kb.io,sideEffects=None,admissionReviewVersions=v1beta1;v1

var (
	_ admission.Defaulter[*MaasCluster] = &MaasCluster{}
	_ admission.Validator[*MaasCluster] = &MaasCluster{}
)

// Default implements admission.Defaulter so a webhook will be registered for the type
func (r *MaasCluster) Default(_ context.Context, _ *MaasCluster) error {
	maasclusterlog.Info("default", "name", r.Name)
	return nil
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type
func (r *MaasCluster) ValidateCreate(_ context.Context, _ *MaasCluster) (admission.Warnings, error) {
	maasclusterlog.Info("validate create", "name", r.Name)
	return nil, nil
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type
func (r *MaasCluster) ValidateUpdate(_ context.Context, oldObj, newObj *MaasCluster) (admission.Warnings, error) {
	maasclusterlog.Info("validate update", "name", newObj.Name)
	if newObj.Spec.DNSDomain != oldObj.Spec.DNSDomain {
		return nil, apierrors.NewBadRequest("changing cluster DNS Domain not allowed")
	}
	if newObj.Spec.APIServerInterfaceTag != oldObj.Spec.APIServerInterfaceTag {
		return nil, apierrors.NewBadRequest("changing cluster APIServerInterfaceTag not allowed")
	}
	return nil, nil
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type
func (r *MaasCluster) ValidateDelete(_ context.Context, _ *MaasCluster) (admission.Warnings, error) {
	maasclusterlog.Info("validate delete", "name", r.Name)
	return nil, nil
}
