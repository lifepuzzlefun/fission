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

package httptrigger

import (
	"fmt"
	"strings"

	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/fission-cli/cliwrapper/cli"
	"github.com/fission/fission/pkg/fission-cli/cmd"
	"github.com/fission/fission/pkg/fission-cli/cmd/spec"
	"github.com/fission/fission/pkg/fission-cli/console"
	flagkey "github.com/fission/fission/pkg/fission-cli/flag/key"
	"github.com/fission/fission/pkg/fission-cli/util"
)

type UpdateSubCommand struct {
	cmd.CommandActioner
	trigger *fv1.HTTPTrigger
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
	htName := input.String(flagkey.HtName)

	_, triggerNamespace, err := opts.GetResourceNamespace(input, flagkey.NamespaceTrigger)
	if err != nil {
		return fmt.Errorf("error in deleting function : %w", err)
	}

	ht, err := opts.Client().FissionClientSet.CoreV1().HTTPTriggers(triggerNamespace).Get(input.Context(), htName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting HTTP trigger: %w", err)
	}

	triggerUrl := input.String(flagkey.HtUrl)
	prefix := input.String(flagkey.HtPrefix)

	if triggerUrl != "" && prefix != "" {
		console.Warn("Prefix will take precedence over URL/RelativeURL")
	}

	if triggerUrl == "/" || prefix == "/" {
		return errors.New("url with only root path is not allowed")
	}
	if triggerUrl != "" && !strings.HasPrefix(triggerUrl, "/") {
		triggerUrl = "/" + triggerUrl
	}
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}

	ht.Spec.RelativeURL = triggerUrl
	ht.Spec.Prefix = &prefix

	if input.IsSet(flagkey.HtKeepPrefix) {
		ht.Spec.KeepPrefix = input.Bool(flagkey.HtKeepPrefix)
	}

	methods := input.StringSlice(flagkey.HtMethod)
	if len(methods) > 0 {
		for _, method := range methods {
			_, err := GetMethod(method)
			if err != nil {
				return err
			}
		}
		ht.Spec.Methods = methods
	}

	if input.IsSet(flagkey.HtFnName) {
		// get the functions and their weights if specified
		functionList := input.StringSlice(flagkey.HtFnName)
		err := util.CheckFunctionExistence(input.Context(), opts.Client(), functionList, triggerNamespace)
		if err != nil {
			console.Warn(err.Error())
		}

		var functionWeightsList []int
		if input.IsSet(flagkey.HtFnWeight) {
			functionWeightsList = input.IntSlice(flagkey.HtFnWeight)
		}

		// set function reference
		functionRef, err := setHtFunctionRef(functionList, functionWeightsList)
		if err != nil {
			return fmt.Errorf("error setting function weight: %w", err)
		}

		ht.Spec.FunctionReference = *functionRef
	}

	if input.IsSet(flagkey.HtIngress) {
		ht.Spec.CreateIngress = input.Bool(flagkey.HtIngress)
	}

	if input.IsSet(flagkey.HtHost) {
		ht.Spec.Host = input.String(flagkey.HtHost)
	}

	if input.IsSet(flagkey.HtIngressRule) || input.IsSet(flagkey.HtIngressAnnotation) || input.IsSet(flagkey.HtIngressTLS) {
		fallbackURL := ""
		if ht.Spec.Prefix != nil && *ht.Spec.Prefix != "" {
			fallbackURL = *ht.Spec.Prefix
		} else {
			fallbackURL = ht.Spec.RelativeURL
		}
		ingress, err := GetIngressConfig(
			input.StringSlice(flagkey.HtIngressAnnotation), input.String(flagkey.HtIngressRule),
			input.String(flagkey.HtIngressTLS), fallbackURL, &ht.Spec.IngressConfig)
		if err != nil {
			return fmt.Errorf("error parsing ingress configuration: %w", err)
		}
		ht.Spec.IngressConfig = *ingress
	}

	opts.trigger = ht

	return nil
}

func (opts *UpdateSubCommand) run(input cli.Input) error {
	if input.Bool(flagkey.SpecSave) {
		err := opts.trigger.Validate()
		if err != nil {
			return fv1.AggregateValidationErrors("HTTPTrigger", err)
		}
		specFile := fmt.Sprintf("route-%s.yaml", opts.trigger.ObjectMeta.Name)
		err = spec.SpecSave(*opts.trigger, specFile, true)
		if err != nil {
			return fmt.Errorf("error saving HTTP trigger spec: %w", err)
		}
		return nil
	}
	err := util.CheckHTTPTriggerDuplicates(input.Context(), opts.Client(), opts.trigger)
	if err != nil {
		return fmt.Errorf("error while creating HTTP Trigger: %w", err)
	}

	_, err = opts.Client().FissionClientSet.CoreV1().HTTPTriggers(opts.trigger.ObjectMeta.Namespace).Update(input.Context(), opts.trigger, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating the HTTP trigger: %w", err)
	}
	fmt.Printf("trigger '%v' updated\n", opts.trigger.ObjectMeta.Name)
	return nil
}
