// SPDX-License-Identifier: Apache-2.0
// SPDX-FileCopyrightText: Zygmunt Krynicki

// Package main implements the plz-run command line utility. The program uses
// D-Bus APIs of systemd, running as the init system, in order to create a
// process elsewhere in the process hierarchy and not as a child process of the
// plz-run program. This allows plz-run to, given proper permissions are
// arranged, to run an unconfined program from a confined context (e.g. under
// restrictive apparmor profile, with an attached seccomp BPF program, with a
// set of eBPF programs attached to the cgroup hierarchy.
//
// The newly started process has connected standard input, output and error
// streams from the streams used to invoke plz-run. The exit code of the remote
// process is relayed.
//
// Supported features:
//
// - running any program with any arguments without shell expansion
// - injecting additional environment variables with the -E switch.
// - running as the given user and group with the -u and -g switches.
//
// Missing features:
//
// - Running under user slice as a user service.
// - Running as a scope.
// - Interacting with systemd --user.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strings"

	"github.com/godbus/dbus/v5"
	"gitlab.com/zygoon/go-cmdr"
)

type EnvList []string

func (e *EnvList) String() string {
	return fmt.Sprintf("%v", *e)
}

func (e *EnvList) Set(value string) error {
	if !strings.Contains(value, "=") {
		return fmt.Errorf("environment variables must have a key=value format, not %q", value)
	}
	*e = append(*e, value)
	return nil
}

func plz(ctx context.Context, args []string) error {
	// Constants related to systemd D-Bus interfaces.
	// Sadly most cannot be strongly typed with go-dbus, as the API relies on untyped strings.
	const (
		dbusPropsIface                                      = "org.freedesktop.DBus.Properties"
		dbusPropsPropertiesChangedMember                    = "PropertiesChanged"
		dbusPropsPropertiesChangedSignal                    = dbusPropsIface + "." + dbusPropsPropertiesChangedMember
		fdoSystemd1BusName                                  = "org.freedesktop.systemd1"
		fdoSystemd1ObjectPath               dbus.ObjectPath = "/org/freedesktop/systemd1"
		fdoSystemd1ManagerIface                             = fdoSystemd1BusName + ".Manager"
		fdoSystemd1StartTransientUnitMethod                 = fdoSystemd1ManagerIface + ".StartTransientUnit"
		fdoSystemd1ManagerJobRemovedMember                  = "JobRemoved"
		fdoSystemd1ManagerJobRemovedSignal                  = fdoSystemd1ManagerIface + "." + fdoSystemd1ManagerJobRemovedMember
	)

	// Parse arguments.
	fl := flag.NewFlagSet("plz", flag.ContinueOnError)
	var user, group string
	var env EnvList
	fl.StringVar(&user, "u", "root", "Ask systemd to use given user")
	fl.StringVar(&group, "g", "root", "Ask systemd to use given group")
	fl.Var(&env, "E", "Ask systemd to inject extra environment variables (can be used multiple times)")
	fl.Usage = func() {
		fmt.Fprintf(fl.Output(), "Usage: %s [OPTIONS] PROG [ARGS]\n", fl.Name())
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() == 0 {
		fl.Usage()
		return flag.ErrHelp
	}

	// Find the program the user wants to run.
	progPath, err := exec.LookPath(fl.Arg(0))
	if err != nil {
		return err
	}
	progArgs := fl.Args()

	// Pick a random number as our unique element of the service we're about to start.
	cookie := rand.Int()

	// Connect to the D-Bus system bus.
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return err
	}
	defer conn.Close()

	// Ask DBus broker to relay the JobRemoved signal as sent by systemd.
	matchJobRemovedExpr := []dbus.MatchOption{
		dbus.WithMatchSender(fdoSystemd1BusName),                 // match the bus name of systemd,
		dbus.WithMatchObjectPath(fdoSystemd1ObjectPath),          // match the sender at the object path of systemd.
		dbus.WithMatchInterface(fdoSystemd1ManagerIface),         // match the Manager interface name.
		dbus.WithMatchMember(fdoSystemd1ManagerJobRemovedMember), // match the JobRemoved interface member.
	}
	conn.AddMatchSignalContext(ctx, matchJobRemovedExpr...)
	defer conn.RemoveMatchSignalContext(ctx, matchJobRemovedExpr...)

	// Ask DBus broker to relay the PropertiesChanged signal as sent by systemd's job.
	matchPropsChangedExpr := []dbus.MatchOption{
		dbus.WithMatchSender(fdoSystemd1BusName), // match the bus name of systemd,
		dbus.WithMatchObjectPath(dbus.ObjectPath(fmt.Sprintf("%s/unit/plz_2drun_2d%d_2eservice", fdoSystemd1ObjectPath, cookie))),
		dbus.WithMatchInterface(dbusPropsIface),                  // match the Properties interface name.
		dbus.WithMatchMember(dbusPropsPropertiesChangedMember),   // match the PropertiesChanged interface member.
		dbus.WithMatchArg(0, "org.freedesktop.systemd1.Service"), // match only messages whose first body item, the interface name, is that of .Service.
	}
	conn.AddMatchSignalContext(ctx, matchPropsChangedExpr...)
	defer conn.RemoveMatchSignalContext(ctx, matchPropsChangedExpr...)

	// Arrange go-dbus to deliver signals to the given channel.
	sigCh := make(chan *dbus.Signal)
	defer close(sigCh)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	// Start the transient unit that corresponds to our workload and get the resulting object path.
	flags := dbus.Flags(0)
	name := fmt.Sprintf("plz-run-%d.service", cookie)
	mode := "fail"
	props := []struct {
		Name  string
		Value dbus.Variant
	}{
		{Name: "Description", Value: dbus.MakeVariant("potato")},
		{Name: "Type", Value: dbus.MakeVariant("oneshot")},
		{Name: "User", Value: dbus.MakeVariant(user)},
		{Name: "Group", Value: dbus.MakeVariant(group)},
		{Name: "StandardInputFileDescriptor", Value: dbus.MakeVariant(dbus.UnixFD(os.Stdin.Fd()))},
		{Name: "StandardOutputFileDescriptor", Value: dbus.MakeVariant(dbus.UnixFD(os.Stdout.Fd()))},
		{Name: "StandardErrorFileDescriptor", Value: dbus.MakeVariant(dbus.UnixFD(os.Stderr.Fd()))},
		{Name: "Environment", Value: dbus.MakeVariant(env)},
		{
			Name: "ExecStart", Value: dbus.MakeVariant([]struct {
				Path          string
				Args          []string
				IgnoreFailure bool
			}{
				{
					Path: progPath,
					Args: progArgs,
				},
			}),
		},
	}
	// The slice of auxiliary units is required by the API but unused.
	var aux []struct {
		Name       string
		Properties []struct {
			Name  string
			Value dbus.Variant
		}
	}

	var ourJobPath dbus.ObjectPath
	obj := conn.Object(fdoSystemd1BusName, fdoSystemd1ObjectPath)
	if err := obj.CallWithContext(ctx, fdoSystemd1StartTransientUnitMethod, flags, name, mode, props, aux).Store(&ourJobPath); err != nil {
		return fmt.Errorf("cannot call StartTransientUnit: %w", err)
	}

	// Iterate through the signals we've received from D-Bus.
	var exitStatus uint32
loop:
	for {
		select {
		case sig := <-sigCh:
			switch sig.Name {
			// When we receive properties changed signal, look for
			// ExecMainStatus field of the .Service interface in order to store
			// the exit code.
			case dbusPropsPropertiesChangedSignal:
				var (
					propsIface       string
					propsChanged     map[string]dbus.Variant
					propsInvalidated []string
				)
				if err := dbus.Store(sig.Body, &propsIface, &propsChanged, &propsInvalidated); err != nil {
					return err
				}

				if val, ok := propsChanged["ExecMainStatus"]; ok {
					if err := val.Store(&exitStatus); err != nil {
						return fmt.Errorf("cannot store ExecMainStatus: %w", err)
					}
				}
			case fdoSystemd1ManagerJobRemovedSignal:
				// When we rececive the JobRemoved signal corresponding to our job, we're done.
				var (
					jobId     uint32
					jobPath   dbus.ObjectPath
					jobUnit   string
					jobResult string
				)
				if err := dbus.Store(sig.Body, &jobId, &jobPath, &jobUnit, &jobResult); err != nil {
					return err
				}
				// The job that we have started has been removed. We can return.
				if jobPath == ourJobPath {
					break loop
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if exitStatus != 0 {
		return cmdr.SilentError(uint8(exitStatus))
	}

	return nil
}

func main() {
	cmdr.RunMain(cmdr.Func(plz), os.Args)
}
