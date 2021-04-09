// Copyright (c) 2021 Banzai Cloud Zrt. All Rights Reserved.

package main

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"emperror.dev/errors"
	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v2"

	"github.com/banzaicloud/cluster-registry-controller/internal/config"
)

type Configuration config.Configuration

func configure() Configuration {
	p := flag.NewFlagSet(FriendlyServiceName, flag.ExitOnError)
	initConfiguration(viper.GetViper(), p)
	_ = p.Parse(os.Args[1:])

	var config Configuration
	bindEnvs(config)
	err := viper.Unmarshal(&config)
	if err != nil {
		setupLog.Error(err, "failed to unmarshal configuration")
		os.Exit(1)
	}

	// Show version if asked for
	if viper.GetBool("version") {
		fmt.Printf("%s version %s (%s) built on %s\n", FriendlyServiceName, version, commitHash, buildDate)
		os.Exit(0)
	}

	// Dump config if asked for
	if viper.GetBool("dump-config") {
		t, err := yaml.Marshal(config)
		if err != nil {
			panic(errors.WrapIf(err, "failed to dump configuration"))
		}
		fmt.Print(string(t))
		os.Exit(0)
	}

	return config
}

func initConfiguration(v *viper.Viper, p *flag.FlagSet) {
	v.AllowEmptyEnv(true)
	p.Init(FriendlyServiceName, flag.ExitOnError)
	p.Usage = func() {
		_, _ = fmt.Fprintf(os.Stderr, "Usage of %s:\n", FriendlyServiceName)
		p.PrintDefaults()
	}

	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()

	p.String("metrics-addr", ":8080", "The address the metric endpoint binds to.")
	p.Bool("devel-mode", false, "Set development mode (mainly for logging).")
	p.Bool("version", false, "Show version information")
	p.Bool("dump-config", false, "Dump configuration to the console")

	p.Bool("enable-leader-election", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	_ = viper.BindPFlag("leader-election.enabled", p.Lookup("enable-leader-election"))

	p.String("leader-election-name", "cluster-registry-leader-election", "Determines the name of the leader election configmap.")
	_ = viper.BindPFlag("leader-election.name", p.Lookup("leader-election-name"))
	p.String("leader-election-namespace", "", "Determines the namespace in which the leader election configmap will be created.")
	_ = viper.BindPFlag("leader-election.namespace", p.Lookup("leader-election-namespace"))

	p.Int("log-verbosity", 0, "Log verbosity")
	_ = viper.BindPFlag("log.verbosity", p.Lookup("log-verbosity"))
	p.String("log-format", "json", "Log format (console, json)")
	_ = viper.BindPFlag("log.format", p.Lookup("log-format"))

	p.String("namespace", "cluster-registry", "Namespace where the controller is running")
	_ = viper.BindPFlag("namespace", p.Lookup("namespace"))

	v.SetDefault("syncController.workerCount", 1)
	v.SetDefault("syncController.rateLimit.maxKeys", 1024)
	v.SetDefault("syncController.rateLimit.maxRatePerSecond", 1)
	v.SetDefault("syncController.rateLimit.maxBurst", 5)
	v.SetDefault("clusterController.workerCount", 2)
	v.SetDefault("clusterController.refreshIntervalSeconds", 0)

	_ = v.BindPFlags(p)
}

func bindEnvs(iface interface{}, parts ...string) {
	ifv := reflect.ValueOf(iface)
	ift := reflect.TypeOf(iface)
	for i := 0; i < ift.NumField(); i++ {
		v := ifv.Field(i)
		t := ift.Field(i)
		tv, ok := t.Tag.Lookup("mapstructure")
		if !ok {
			continue
		}
		switch v.Kind() { //nolint:exhaustive
		case reflect.Struct:
			bindEnvs(v.Interface(), append(parts, tv)...)
		default:
			err := viper.BindEnv(strings.Join(append(parts, tv), "."))
			if err != nil {
				panic(errors.WrapIf(err, "could not bind env variable"))
			}
		}
	}
}