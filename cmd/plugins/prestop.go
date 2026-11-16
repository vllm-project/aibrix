/*
Copyright 2026 The Aibrix Team.

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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vllm-project/aibrix/pkg/plugins/gateway/drain"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
)

func runPreStop(args []string) int {
	// Exec hook output is separate from container logs. Match the original
	// hook's PID 1 logging, falling back when that descriptor is inaccessible.
	var output io.Writer = os.Stderr
	if file, err := os.OpenFile("/proc/1/fd/2", os.O_WRONLY|os.O_APPEND, 0); err == nil {
		defer func() { _ = file.Close() }()
		output = file
	}
	return executePreStop(args, output)
}

func executePreStop(args []string, output io.Writer) int {
	var opts drain.Options
	fs := flag.NewFlagSet("prestop", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(output, "Usage: gateway-plugins prestop [flags]")
		fs.PrintDefaults()
	}
	fs.StringVar(&opts.MetricsURL, "metrics-url", "http://127.0.0.1:8080/metrics", "This Pod's metrics URL.")
	fs.DurationVar(&opts.PropagationDelay, "propagation-delay", 10*time.Second, "Endpoint propagation wait.")
	fs.DurationVar(&opts.Timeout, "timeout", 300*time.Second, "Total hook budget, including propagation and fetches.")
	fs.DurationVar(&opts.PollInterval, "poll-interval", 2*time.Second, "Wait between metrics fetches.")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	logger := textlogger.NewLogger(textlogger.NewConfig(textlogger.Output(output))).WithName("gateway preStop")
	if fs.NArg() != 0 {
		logger.Error(nil, "unexpected positional arguments")
		return 2
	}
	if err := opts.Validate(); err != nil {
		logger.Error(err, "invalid configuration")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := drain.Wait(klog.NewContext(ctx, logger), opts); err != nil {
		logger.Error(err, "preStop interrupted")
		return 1
	}
	return 0
}
