// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	. "github.com/pingcap/check"
)

var _ = Suite(&testConfigSuite{})

type testConfigSuite struct{}

func (s *testConfigSuite) TestTLS(c *C) {
	cfg := NewConfig()
	tls, err := cfg.Security.ToTLSConfig()
	c.Assert(err, IsNil)
	c.Assert(tls, IsNil)
}

func (s *testConfigSuite) TestBadFormatJoinAddr(c *C) {
	cfg := NewTestSingleConfig()
	cfg.Join = "127.0.0.1:2379" // Wrong join addr without scheme.
	c.Assert(cfg.adjust(), NotNil)
}
