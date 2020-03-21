// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// +build !windows

package timeutil

/*
#include <time.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	errors2 "github.com/pkg/errors"
)

var (
	clockDevice             *os.File
	clockDeviceId           uintptr
	clockDeviceCallCount    uint64
	clockDeviceHlcCallCount uint64
	hlcOnly                 bool
)

func ClockDeviceNow() time.Time {
	var ts C.struct_timespec
	_, err := C.clock_gettime(C.clockid_t(clockDeviceId), &ts)
	if err != nil {
		panic(err)
	}
	return time.Unix(int64(ts.tv_sec), int64(ts.tv_nsec))
}

func SetHlcOnly() {
	hlcOnly = true
}

// Use the given device as a clock source.
func UseClockDevice(clockDeviceName string) error {
	if clockDeviceId != 0 {
		return errors.New("change of clock device not supported")
	}
	var err error
	clockDevice, err = os.Open(clockDeviceName)
	if err == nil {
		clockDeviceFd := clockDevice.Fd()
		clockId := (^clockDeviceFd << 3) | 3
		log.Printf("Opened clock device %s with fd %x, mod_fd %x\n",
			clockDeviceName,
			clockDeviceFd,
			clockId,
		)
		var ts C.struct_timespec
		_, err := C.clock_gettime(C.clockid_t(clockId), &ts)
		if err != nil {
			return errors2.Wrap(err, "UseClockDevice: error calling clock_gettime")
		}
		clockDeviceId = clockId
	} else {
		return fmt.Errorf("Can't open %s, err %s\n", clockDeviceName, err)
	}
	return nil
}

// return the call count for the timeutil.Now() and separately the counts from Hlc
func NowCallCount() (uint64, uint64) {
	return clockDeviceCallCount, clockDeviceHlcCallCount
}

// Now returns the current UTC time.
func Now() time.Time {
	atomic.AddUint64(&clockDeviceCallCount, 1)
	if clockDeviceId != 0 && !hlcOnly {
		return ClockDeviceNow().UTC()
	} else {
		return time.Now().UTC()
	}
}

// only called by Hlc
func HlcNow() time.Time {
	atomic.AddUint64(&clockDeviceCallCount, 1)
	atomic.AddUint64(&clockDeviceHlcCallCount, 1)
	if clockDeviceId != 0 {
		return ClockDeviceNow().UTC()
	} else {
		return time.Now().UTC()
	}
}
