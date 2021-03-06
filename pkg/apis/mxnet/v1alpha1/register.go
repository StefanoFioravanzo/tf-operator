// Copyright 2018 The Kubeflow Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/sabhiram/go-tracey"
)

var Exit, Enter = tracey.New(nil)

var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

const (
	// GroupName is the group name use in this package.
	GroupName = "fioravanzo.org"
	// TFJobResourceKind is the kind name.
	TFJobResourceKind = "MXJob"
	// GroupVersion is the version.
	GroupVersion = "v1alpha1"
)

// SchemeGroupVersion is the group version used to register these objects.
var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: CRDVersion}

func init() {
	defer Exit(Enter("register.go $FN"))
	// We only register manually written functions here. The registration of the
	// generated functions takes place in the generated files. The separation
	// makes the code compile even when the generated files are missing.
	// TODO: Understand why here we need to call this function. Why do we need a defaults.go file? Why does not generate the deepcopy thourgh the defaulter code-gen?
	//SchemeBuilder.Register(addDefaultingFuncs)
}

// Resource takes an unqualified resource and returns a Group-qualified GroupResource.
func Resource(resource string) schema.GroupResource {
	defer Exit(Enter())
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

// addKnownTypes adds the set of types defined in this package to the supplied scheme.
func addKnownTypes(scheme *runtime.Scheme) error {
	defer Exit(Enter("register.go $FN"))
	scheme.AddKnownTypes(SchemeGroupVersion,
		&MXJob{},
		&MXJobList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}
