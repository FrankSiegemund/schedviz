//
// Copyright 2019 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS-IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
//
package sched

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"github.com/google/schedviz/tracedata/trace"
)

// eventLoader provides a flexible interface for generating threadTransitions
// from raw trace.Events.  A new eventLoader may be populated with a number of
// loader functions, each associated with a distinct tracepoint event name.
// Each loader function converts a single event of the specified type into
// zero or more threadTransitions, describing the state or CPU transitions that
// event described on individual threads.  The threadTransitions thus produced
// may have Unknown values which may then be inferred from subsequent passes,
// and reduced confidence in the event types may be reflected in a generated
// threadTransition by signaling that, in a conflict with other
// threadTransitions, this one should be dropped.
type eventLoader struct {
	stringBank *stringBank
	loaders    map[string]func(*trace.Event, *ThreadTransitionSetBuilder) error
}

// newEventLoader returns a new, empty, eventLoader.
func newEventLoader(loaders map[string]func(*trace.Event, *ThreadTransitionSetBuilder) error, stringBank *stringBank) (*eventLoader, error) {
	if len(loaders) == 0 {
		return nil, errors.New("an empty eventLoader cannot generate threadTransitions")
	}
	return &eventLoader{
		stringBank: stringBank,
		loaders:    loaders,
	}, nil
}

// threadTransitions returns the list of threadTransitions generated by the
// receiver's loaders for the provided trace.Event.
func (el *eventLoader) threadTransitions(ev *trace.Event) ([]*threadTransition, error) {
	if ev.Clipped {
		return nil, nil
	}
	ttsb := newThreadTransitionSetBuilder(el.stringBank)
	loader, ok := el.loaders[ev.Name]
	if !ok {
		return nil, nil
	}
	if err := loader(ev, ttsb); err != nil {
		return nil, err
	}
	return ttsb.transitions(), nil
}

// MissingFieldError is used to report a missing field.
func MissingFieldError(fieldName string, ev *trace.Event) error {
	return status.Errorf(codes.NotFound, "field '%s' not found for event %d", fieldName, ev.Index)
}

// LoadSchedMigrateTask loads a sched::sched_migrate_task event.
func LoadSchedMigrateTask(ev *trace.Event, ttsb *ThreadTransitionSetBuilder) error {
	pid, ok := ev.NumberProperties["pid"]
	if !ok {
		return MissingFieldError("pid", ev)
	}
	comm := ev.TextProperties["comm"]
	prio, ok := ev.NumberProperties["prio"]
	priority := Priority(prio)
	if !ok {
		priority = UnknownPriority
	}
	origCPU, ok := ev.NumberProperties["orig_cpu"]
	if !ok {
		return MissingFieldError("orig_cpu", ev)
	}
	destCPU, ok := ev.NumberProperties["dest_cpu"]
	if !ok {
		return MissingFieldError("dest_cpu", ev)
	}
	// sched:sched_migrate_task produces a single thread transition, from the PID
	// backwards on the original CPU and forwards on the destination CPU.
	ttsb.WithTransition(ev.Index, ev.Timestamp, PID(pid)).
		WithPrevCommand(comm).
		WithNextCommand(comm).
		WithPrevPriority(priority).
		WithNextPriority(priority).
		WithPrevCPU(CPUID(origCPU)).
		WithNextCPU(CPUID(destCPU))
	return nil
}

// SwitchData comprises the data extracted from a raw sched_switch event.
// In its members, Next and Prev refer to the switched-in and switched-out
// threads respectively.
type SwitchData struct {
	NextPID, PrevPID           PID
	NextComm, PrevComm         string
	NextPriority, PrevPriority Priority
	PrevState                  ThreadState
}

// LoadSwitchData loads the data from a sched_switch event, converting all
// fields to suitable types, and returns a SwitchData struct.
func LoadSwitchData(ev *trace.Event) (*SwitchData, error) {
	ret := &SwitchData{}
	nextPID, ok := ev.NumberProperties["next_pid"]
	if !ok {
		return nil, MissingFieldError("next_pid", ev)
	}
	ret.NextPID = PID(nextPID)
	ret.NextComm = ev.TextProperties["next_comm"]
	nextPrio, ok := ev.NumberProperties["next_prio"]
	ret.NextPriority = Priority(nextPrio)
	if !ok {
		ret.NextPriority = UnknownPriority
	}
	prevPID, ok := ev.NumberProperties["prev_pid"]
	if !ok {
		return nil, MissingFieldError("prev_pid", ev)
	}
	ret.PrevPID = PID(prevPID)
	ret.PrevComm = ev.TextProperties["prev_comm"]
	prevPrio, ok := ev.NumberProperties["prev_prio"]
	ret.PrevPriority = Priority(prevPrio)
	if !ok {
		ret.PrevPriority = UnknownPriority
	}
	// The new PID's state is assumed to be RUNNING_STATE, and the old PID's task
	// state will reveal whether it's WAITING_STATE (prev_state == 0,
	// TASK_RUNNING) or SLEEPING_STATE (otherwise); The possible values of
	// prevTaskState are defined in sched.h in the kernel.
	prevTaskState, ok := ev.NumberProperties["prev_state"]
	if !ok {
		return nil, MissingFieldError("prev_state", ev)
	}
	ret.PrevState = WaitingState
	if prevTaskState != 0 {
		ret.PrevState = SleepingState
	}
	return ret, nil
}

// LoadSchedSwitch loads a sched::sched_switch event.
func LoadSchedSwitch(ev *trace.Event, ttsb *ThreadTransitionSetBuilder) error {
	sd, err := LoadSwitchData(ev)
	if err != nil {
		return err
	}
	// sched:sched_switch produces two thread transitions:
	// * The next PID backwards and forwards on the reporting CPU and forwards in
	//   Running state,
	// * The previous PID backwards and forwards on the reporting CPU, backwards
	//   in Running state, and forwards in Sleeping or Waiting state, depending on
	//   its prev_state.
	ttsb.WithTransition(ev.Index, ev.Timestamp, sd.NextPID).
		WithPrevCommand(sd.NextComm).
		WithNextCommand(sd.NextComm).
		WithPrevPriority(sd.NextPriority).
		WithNextPriority(sd.NextPriority).
		WithPrevCPU(CPUID(ev.CPU)).
		WithNextCPU(CPUID(ev.CPU)).
		WithNextState(RunningState)
	ttsb.WithTransition(ev.Index, ev.Timestamp, sd.PrevPID).
		WithPrevCommand(sd.PrevComm).
		WithNextCommand(sd.PrevComm).
		WithPrevPriority(sd.PrevPriority).
		WithNextPriority(sd.PrevPriority).
		WithPrevCPU(CPUID(ev.CPU)).
		WithNextCPU(CPUID(ev.CPU)).
		WithPrevState(RunningState).
		WithNextState(sd.PrevState)
	return nil
}

// LoadSchedWakeup loads a sched::sched_wakeup or sched::sched_wakeup_new event.
func LoadSchedWakeup(ev *trace.Event, ttsb *ThreadTransitionSetBuilder) error {
	pid, ok := ev.NumberProperties["pid"]
	if !ok {
		return MissingFieldError("pid", ev)
	}
	comm := ev.TextProperties["comm"]
	prio, ok := ev.NumberProperties["prio"]
	priority := Priority(prio)
	if !ok {
		priority = UnknownPriority
	}
	targetCPU, ok := ev.NumberProperties["target_cpu"]
	if !ok {
		return MissingFieldError("target_cpu", ev)
	}
	// sched:sched_wakeup and sched:sched_wakeup_new produce a single thread
	// transition, which expects the targetCPU both before and after the
	// transition, and expects the state after the transition to be Waiting.
	//
	// sched_wakeups are quite prone to misbehavior.  They are frequently produced
	// as part of an interrupt, so they may appear misordered relative to other
	// events, and they can be reported by a different CPU than their target CPU.
	// Moreover, wakeups can occur on threads that are already running.
	// Therefore, all assertions sched_wakeup transitions make -- CPU backwards
	// and forwards, and state forwards -- are relaxed, such that sched_wakeups
	// that disagree with other events on these assertions are dropped.
	ttsb.WithTransition(ev.Index, ev.Timestamp, PID(pid)).
		WithPrevCommand(comm).
		WithNextCommand(comm).
		WithPrevPriority(priority).
		WithNextPriority(priority).
		WithPrevCPU(CPUID(targetCPU)).
		WithNextCPU(CPUID(targetCPU)).
		WithNextState(WaitingState).
		OnBackwardsCPUConflict(Drop).
		OnForwardsCPUConflict(Drop).
		OnForwardsStateConflict(Drop)
	return nil
}

// DefaultEventLoaders is a set of event loader functions for standard
// scheduling tracepoints.
func DefaultEventLoaders() map[string]func(*trace.Event, *ThreadTransitionSetBuilder) error {
	return map[string]func(*trace.Event, *ThreadTransitionSetBuilder) error{
		"sched_migrate_task": LoadSchedMigrateTask,
		"sched_switch":       LoadSchedSwitch,
		"sched_wakeup":       LoadSchedWakeup,
		"sched_wakeup_new":   LoadSchedWakeup,
	}
}

// LoadSchedSwitchWithSynthetics loads a sched::sched_switch event from a trace
// that lacks other events that could signal thread state or CPU changes.
// Wherever a state or CPU transition is missing, a synthetic transition will
// be inserted midway between the two adjacent known transitions.
func LoadSchedSwitchWithSynthetics(ev *trace.Event, ttsb *ThreadTransitionSetBuilder) error {
	sd, err := LoadSwitchData(ev)
	if err != nil {
		return err
	}
	ttsb.WithTransition(ev.Index, ev.Timestamp, sd.NextPID).
		WithPrevCommand(sd.NextComm).
		WithNextCommand(sd.NextComm).
		WithPrevPriority(sd.NextPriority).
		WithNextPriority(sd.NextPriority).
		WithPrevCPU(CPUID(ev.CPU)).
		WithNextCPU(CPUID(ev.CPU)).
		WithPrevState(WaitingState).
		WithNextState(RunningState).
		OnForwardsCPUConflict(InsertSynthetic).
		OnBackwardsCPUConflict(InsertSynthetic).
		OnForwardsStateConflict(InsertSynthetic).
		OnBackwardsStateConflict(InsertSynthetic)
	ttsb.WithTransition(ev.Index, ev.Timestamp, sd.PrevPID).
		WithPrevCommand(sd.PrevComm).
		WithNextCommand(sd.PrevComm).
		WithPrevPriority(sd.PrevPriority).
		WithNextPriority(sd.PrevPriority).
		WithPrevCPU(CPUID(ev.CPU)).
		WithNextCPU(CPUID(ev.CPU)).
		WithPrevState(RunningState).
		WithNextState(sd.PrevState).
		OnForwardsCPUConflict(InsertSynthetic).
		OnBackwardsCPUConflict(InsertSynthetic).
		OnForwardsStateConflict(InsertSynthetic).
		OnBackwardsStateConflict(InsertSynthetic)
	return nil
}

// SwitchOnlyLoaders is a set of loaders suitable for use on traces in
// which scheduling behavior is only attested by sched_switch events.
func SwitchOnlyLoaders() map[string]func(*trace.Event, *ThreadTransitionSetBuilder) error {
	return map[string]func(*trace.Event, *ThreadTransitionSetBuilder) error{
		"sched_switch": LoadSchedSwitchWithSynthetics,
	}
}