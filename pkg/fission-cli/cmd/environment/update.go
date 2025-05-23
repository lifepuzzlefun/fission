/*
Copyright 2019 The Fission Authors.

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

package environment

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/hashicorp/go-multierror"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/fission-cli/cliwrapper/cli"
	"github.com/fission/fission/pkg/fission-cli/cmd"
	"github.com/fission/fission/pkg/fission-cli/cmd/spec"
	"github.com/fission/fission/pkg/fission-cli/console"
	flagkey "github.com/fission/fission/pkg/fission-cli/flag/key"
	"github.com/fission/fission/pkg/fission-cli/util"
	"github.com/fission/fission/pkg/utils"
)

type UpdateSubCommand struct {
	cmd.CommandActioner
	env *fv1.Environment
}

func Update(input cli.Input) error {
	return (&UpdateSubCommand{}).do(input)
}

func (opts *UpdateSubCommand) do(input cli.Input) error {
	err := opts.complete(input)
	if err != nil {
		return err
	}
	return opts.run(input)
}

func (opts *UpdateSubCommand) complete(input cli.Input) (err error) {

	_, currentContextNS, err := opts.GetResourceNamespace(input, flagkey.NamespaceEnvironment)
	if err != nil {
		return fmt.Errorf("error creating environment: %w", err)
	}
	env, err := opts.Client().FissionClientSet.CoreV1().Environments(currentContextNS).Get(input.Context(), input.String(flagkey.EnvName), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error finding environment: %w", err)
	}

	env, err = updateExistingEnvironmentWithCmd(env, input)
	if err != nil {
		return err
	}

	opts.env = env

	err = util.ApplyLabelsAndAnnotations(input, &opts.env.ObjectMeta)
	if err != nil {
		return err
	}
	return nil
}

func (opts *UpdateSubCommand) run(input cli.Input) error {
	m := opts.env.ObjectMeta
	if input.Bool(flagkey.SpecSave) {
		err := opts.env.Validate()
		if err != nil {
			return fv1.AggregateValidationErrors("Environment", err)
		}

		specFile := fmt.Sprintf("env-%s.yaml", m.Name)
		err = spec.SpecSave(*opts.env, specFile, true)
		if err != nil {
			return fmt.Errorf("error saving environment spec: %w", err)
		}
		return nil
	}
	enew, err := opts.Client().FissionClientSet.CoreV1().Environments(opts.env.ObjectMeta.Namespace).Update(input.Context(), opts.env, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating environment: %w", err)
	}

	fmt.Printf("environment '%v' updated\n", enew.Name)
	return nil
}

// updateExistingEnvironmentWithCmd updates a existing environment's value based on CLI input.
func updateExistingEnvironmentWithCmd(env *fv1.Environment, input cli.Input) (*fv1.Environment, error) {
	e := utils.MultiErrorWithFormat()

	if input.IsSet(flagkey.EnvImage) {
		env.Spec.Runtime.Image = input.String(flagkey.EnvImage)
	}

	if input.IsSet(flagkey.EnvBuilderImage) {
		env.Spec.Builder.Image = input.String(flagkey.EnvBuilderImage)
	}

	if input.IsSet(flagkey.EnvBuildcommand) {
		env.Spec.Builder.Command = input.String(flagkey.EnvBuildcommand)
	}

	if env.Spec.Version == 1 && (len(env.Spec.Builder.Image) > 0 || len(env.Spec.Builder.Command) > 0) {
		e = multierror.Append(e, errors.New("version 1 Environments do not support builders. Must specify --version=2"))
	}
	if input.IsSet(flagkey.EnvExternalNetwork) {
		env.Spec.AllowAccessToExternalNetwork = input.Bool(flagkey.EnvExternalNetwork)
	}

	if input.IsSet(flagkey.EnvPoolsize) {
		env.Spec.Poolsize = input.Int(flagkey.EnvPoolsize)
		if env.Spec.Poolsize < 1 {
			console.Warn("poolsize is not positive, if you are using pool manager please set positive value")
		}
	}

	if input.IsSet(flagkey.EnvGracePeriod) {
		env.Spec.TerminationGracePeriod = input.Int64(flagkey.EnvGracePeriod)
	}

	if input.IsSet(flagkey.EnvKeeparchive) {
		env.Spec.KeepArchive = input.Bool(flagkey.EnvKeeparchive)
	}

	if input.IsSet(flagkey.EnvImagePullSecret) {
		env.Spec.ImagePullSecret = input.String(flagkey.EnvImagePullSecret)
	}

	env.Spec.Resources.Requests = make(v1.ResourceList)
	env.Spec.Resources.Limits = make(v1.ResourceList)

	if input.IsSet(flagkey.RuntimeMincpu) {
		mincpu := input.Int(flagkey.RuntimeMincpu)
		cpuRequest, err := resource.ParseQuantity(strconv.Itoa(mincpu) + "m")
		if err != nil {
			e = multierror.Append(e, fmt.Errorf("failed to parse mincpu: %w", err))
		}
		env.Spec.Resources.Requests[v1.ResourceCPU] = cpuRequest
	}

	if input.IsSet(flagkey.RuntimeMaxcpu) {
		maxcpu := input.Int(flagkey.RuntimeMaxcpu)
		cpuLimit, err := resource.ParseQuantity(strconv.Itoa(maxcpu) + "m")
		if err != nil {
			e = multierror.Append(e, fmt.Errorf("failed to parse maxcpu: %w", err))
		}
		env.Spec.Resources.Limits[v1.ResourceCPU] = cpuLimit
	}

	if input.IsSet(flagkey.RuntimeMinmemory) {
		minmem := input.Int(flagkey.RuntimeMinmemory)
		memRequest, err := resource.ParseQuantity(strconv.Itoa(minmem) + "Mi")
		if err != nil {
			e = multierror.Append(e, fmt.Errorf("failed to parse minmemory: %w", err))
		}
		env.Spec.Resources.Requests[v1.ResourceMemory] = memRequest
	}

	if input.IsSet(flagkey.RuntimeMaxmemory) {
		maxmem := input.Int(flagkey.RuntimeMaxmemory)
		memLimit, err := resource.ParseQuantity(strconv.Itoa(maxmem) + "Mi")
		if err != nil {
			e = multierror.Append(e, fmt.Errorf("failed to parse maxmemory: %w", err))
		}
		env.Spec.Resources.Limits[v1.ResourceMemory] = memLimit
	}

	if input.IsSet(flagkey.EnvRuntime) {
		runtimeEnvParams := input.StringSlice(flagkey.EnvRuntime)
		runtimeEnvList := util.GetEnvVarFromStringSlice(runtimeEnvParams)
		env.Spec.Runtime.Container.Env = runtimeEnvList
	}

	limitCPU := env.Spec.Resources.Limits[v1.ResourceCPU]
	requestCPU := env.Spec.Resources.Requests[v1.ResourceCPU]

	if limitCPU.IsZero() && !requestCPU.IsZero() {
		env.Spec.Resources.Limits[v1.ResourceCPU] = requestCPU
	} else if limitCPU.Cmp(requestCPU) < 0 {
		e = multierror.Append(e, fmt.Errorf("minCPU (%v) cannot be greater than MaxCPU (%v)", requestCPU.String(), limitCPU.String()))
	}

	limitMem := env.Spec.Resources.Limits[v1.ResourceMemory]
	requestMem := env.Spec.Resources.Requests[v1.ResourceMemory]

	if limitMem.IsZero() && !requestMem.IsZero() {
		env.Spec.Resources.Limits[v1.ResourceMemory] = requestMem
	} else if limitMem.Cmp(requestMem) < 0 {
		e = multierror.Append(e, fmt.Errorf("minMemory (%v) cannot be greater than MaxMemory (%v)", requestMem.String(), limitMem.String()))
	}

	if e.ErrorOrNil() != nil {
		return nil, e.ErrorOrNil()
	}

	return env, nil
}
