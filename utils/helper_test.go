/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */
package utils

import "testing"

func TestClassLoaderID(t *testing.T) {
	if ClassLoaderID() != 0 {
		t.Errorf("wrong ClassLoaderID, want=%d, got=%d", 0, ClassLoaderID())
	}
}

func TestLocalIP(t *testing.T) {
	ip := LocalIP()
	if ip[0] == 0 && ip[1] == 0 && ip[2] == 0 && ip[3] == 0 {
		t.Errorf("failed to get host public ip4 address")
	} else {
		t.Logf("ip4 address: %v", ip)
	}
}

func BenchmarkMessageClientID(b *testing.B) {
	for i := 0; i < b.N; i++ {
		MessageClientID()
	}
}
