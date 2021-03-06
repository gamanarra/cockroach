// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Radu Berinde (radu@cockroachlabs.com)

package sql

import (
	"testing"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/base"
	"github.com/cockroachdb/cockroach/testutils/serverutils"
	"github.com/cockroachdb/cockroach/util/leaktest"
)

// Temporary proof-of-concept test that uses the testingshim to set up a test
// server from the sql package.
func TestPOC(t *testing.T) {
	defer leaktest.AfterTest(t)()

	s, _, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer s.Stopper().Stop()

	err := kvDB.Put(context.TODO(), "testkey", "testval")
	if err != nil {
		t.Fatal(err)
	}
	kv, err := kvDB.Get(context.TODO(), "testkey")
	if err != nil {
		t.Fatal(err)
	}
	if kv.PrettyValue() != `"testval"` {
		t.Errorf(`Invalid Get result: %s, expected "testval"`, kv.PrettyValue())
	}
}
