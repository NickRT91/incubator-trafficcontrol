package health

/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

import (
	"time"
)

// Event represents an event change in aggregated data. For example, a cache being marked as unavailable.
type Event struct {
	Time        time.Time `json:"-"`
	Index       uint64    `json:"index"`
	Unix        int64     `json:"time"`
	Description string    `json:"description"`
	Name        string    `json:"name"`
	Hostname    string    `json:"hostname"`
	Type        string    `json:"type"`
	Available   bool      `json:"isAvailable"`
}
