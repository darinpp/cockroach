// Copyright 2021 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package denylist

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func newTestFileCfg(t *testing.T, ctx context.Context) (Service, *viper.Viper, string, func()) {
	dir, err := ioutil.TempDir(os.TempDir(), "denylist-test")
	if err != nil {
		t.Fatal(err)
	}
	fn := filepath.Join(dir, "denylist.yml")
	err = ioutil.WriteFile(fn, []byte{}, 0644)
	require.NoError(t, err)

	v, err := NewViperCfgFromFile(fn)
	require.NoError(t, err)

	return NewViperDenyList(ctx, v, WithPollInterval(time.Millisecond)), v, fn, func() { os.Remove(dir) }
}

func TestViperDenyList(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dl, v, cfgFile, cleanup := newTestFileCfg(t, ctx)
	defer cleanup()

	e, err := dl.Denied("123")
	require.NoError(t, err)
	require.True(t, e == nil)

	v.Set("123", "over quota")
	e, err = dl.Denied("123")
	require.NoError(t, err)
	require.Equal(t, &Entry{"over quota"}, e)

	err = ioutil.WriteFile(cfgFile, []byte("456: denied"), 0644)
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	e, err = dl.Denied("456")
	require.NoError(t, err)
	require.Equal(t, &Entry{Reason: "denied"}, e)
}
