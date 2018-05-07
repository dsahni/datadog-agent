// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2018 Datadog, Inc.

// +build cpython

package py

import (
	"errors"
	"fmt"

	autodiscovery "github.com/DataDog/datadog-agent/pkg/autodiscovery/config"
	"github.com/DataDog/datadog-agent/pkg/collector/check"
	"github.com/DataDog/datadog-agent/pkg/collector/loaders"
	"github.com/sbinet/go-python"

	log "github.com/cihub/seelog"
)

// const things
const agentCheckClassName = "AgentCheck"
const agentCheckModuleName = "checks"

// PythonCheckLoader is a specific loader for checks living in Python modules
type PythonCheckLoader struct {
	agentCheckClass *python.PyObject
}

// NewPythonCheckLoader creates an instance of the Python checks loader
func NewPythonCheckLoader() (*PythonCheckLoader, error) {
	// Lock the GIL and release it at the end of the run
	glock := newStickyLock()
	defer glock.unlock()

	agentCheckModule := python.PyImport_ImportModule(agentCheckModuleName)
	if agentCheckModule == nil {
		log.Errorf("Unable to import Python module: %s", agentCheckModuleName)
		return nil, fmt.Errorf("unable to initialize AgentCheck module")
	}
	defer agentCheckModule.DecRef()

	agentCheckClass := agentCheckModule.GetAttrString(agentCheckClassName) // don't `DecRef` for now since we keep the ref around in the returned PythonCheckLoader
	if agentCheckClass == nil {
		log.Errorf("Unable to import %s class from Python module: %s", agentCheckClassName, agentCheckModuleName)
		return nil, errors.New("unable to initialize AgentCheck class")
	}

	return &PythonCheckLoader{agentCheckClass}, nil
}

// Load tries to import a Python module with the same name found in config.Name, searches for
// subclasses of the AgentCheck class and returns the corresponding Check
func (cl *PythonCheckLoader) Load(config autodiscovery.Config) ([]check.Check, error) {
	checks := []check.Check{}
	moduleName := config.Name
	whlModuleName := fmt.Sprintf("datadog_checks.%s", config.Name)

	// Looking for wheels first
	modules := []string{whlModuleName, moduleName}

	// Lock the GIL while working with go-python directly
	glock := newStickyLock()

	// Platform-specific preparation
	var err error
	err = platformLoaderPrep()
	if err != nil {
		return nil, err
	}
	defer platformLoaderDone()

	var pyErr string
	var checkModule *python.PyObject
	for _, name := range modules {
		// import python module containing the check
		checkModule = python.PyImport_ImportModule(name)
		if checkModule != nil {
			break
		}

		pyErr, err = glock.getPythonError()
		if err != nil {
			err = fmt.Errorf("An error occurred while loading the python module and couldn't be formatted: %v", err)
		} else {
			err = errors.New(pyErr)
		}
		log.Debugf("Unable to load python module - %s: %v", name, err)
	}

	// all failed, return error for last failure
	if checkModule == nil {
		defer glock.unlock()
		return nil, err
	}

	// Try to find a class inheriting from AgentCheck within the module
	checkClass, err := findSubclassOf(cl.agentCheckClass, checkModule, glock)
	checkModule.DecRef()
	glock.unlock()
	if err != nil {
		msg := fmt.Sprintf("Unable to find a check class in the module: %v", err)
		return checks, errors.New(msg)
	}

	// Get an AgentCheck for each configuration instance and add it to the registry
	for _, i := range config.Instances {
		check := NewPythonCheck(moduleName, checkClass)
		// The GIL should be unlocked at this point, `check.Configure` uses its own stickyLock and stickyLocks must not be nested
		if err := check.Configure(i, config.InitConfig); err != nil {
			log.Errorf("py.loader: could not configure check '%s': %s", moduleName, err)
			continue
		}
		checks = append(checks, check)
	}
	glock = newStickyLock()
	defer glock.unlock()
	checkClass.DecRef()

	log.Debugf("python loader: done loading check %s", moduleName)
	return checks, nil
}

func (cl *PythonCheckLoader) String() string {
	return "Python Check Loader"
}

func init() {
	factory := func() (check.Loader, error) {
		return NewPythonCheckLoader()
	}

	loaders.RegisterLoader(10, factory)
}
