//  Copyright (c) 2015 Rackspace
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
//  implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package objectserver

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/troubling/hummingbird/common"
	"github.com/troubling/hummingbird/common/fs"
	"github.com/troubling/hummingbird/common/ring"
)

// walk through the nursery and check every object (and tombstone) if it is
// fully replicated.  that means HEAD other primaries for the object. if the
// ALL other primaries return 2xx/matching x-timestamp then mv local copy to
// stable directory. the other servers will mv their own copies as they find
// them. That means the remote copies can be either in nursery or stable as
// HEAD requests always check both

// if the object is not on one of the other primaries or has mismatched
// timestamps then leave the object alone and wait for existing replicator
// daemon to fully replicate it.

// the existing replicator will not really be changed. but it only walks the
// nursery. the stable objects dir will only be affected by priority
// replication calls sent to it by andrewd (or ops). these calls will happen
// within the existing framework except that they will reference the stable
// dirs.  these priority rep calls will be triggered by ring changes,
// manually by ops, in response to dispersion scan missing objects.  since
// long unmounted drives are zeroed out in ring by the andrewd/drivewatch
// these are also handled once the ring is changed

// PUTs / POST / DELETE will only write to nursery GET / HEAD will check
// nursery and stable on every request. i think it has to check both. it
// seems possible that a more recent copy could be in stable with all the
// handoffs / replication going on in the nursery

// once tombstones get put into stable they can set there indefinitely- i
// guess auditor can clean them up?
const nurseryObjectSleep = 10 * time.Millisecond

type nurseryDevice struct {
	r         *Replicator
	passStart time.Time
	dev       *ring.Device
	policy    int
	oring     ring.Ring
	canchan   chan struct{}
	client    http.Client
	objEngine ObjectEngine
}

func (nrd *nurseryDevice) updateStat(stat string, amount int64) {
	key := deviceKey(nrd.dev, nrd.policy)
	nrd.r.updateStat <- statUpdate{"object-nursery", key, stat, amount}
}

func (nrd *nurseryDevice) validateObj(obj Object) bool {
	metadata := obj.Metadata()
	ns := strings.SplitN(metadata["name"], "/", 4)
	if len(ns) != 4 {
		nrd.r.logger.Error("invalid metadata name", zap.String("name", metadata["name"]))
		return false
	}
	partition := nrd.oring.GetPartition(ns[1], ns[2], ns[3])
	if _, handoff := nrd.oring.GetJobNodes(partition, nrd.dev.Id); handoff {
		return false
	}
	nodes := nrd.oring.GetNodes(partition)
	goodNodes := uint64(0)
	for _, device := range nodes {
		if device.Ip == nrd.dev.Ip && device.Port == nrd.dev.Port && device.Device == device.Device {
			continue
		}
		url := fmt.Sprintf("http://%s:%d/%s/%d%s", device.Ip, device.Port, device.Device, partition, common.Urlencode(metadata["name"]))
		req, err := http.NewRequest("HEAD", url, nil)
		req.Header.Set("X-Backend-Storage-Policy-Index", strconv.FormatInt(int64(nrd.policy), 10))
		req.Header.Set("User-Agent", "nursery-stabilizer")
		resp, err := nrd.client.Do(req)

		if err == nil && (resp.StatusCode/100 == 2 || resp.StatusCode == 404) &&
			resp.Header.Get("X-Backend-Data-Timestamp") != "" &&
			resp.Header.Get("X-Backend-Data-Timestamp") ==
				metadata["X-Backend-Data-Timestamp"] &&
			resp.Header.Get("X-Backend-Meta-Timestamp") ==
				metadata["X-Backend-Meta-Timestamp"] {
			goodNodes++
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	return goodNodes+1 == nrd.oring.ReplicaCount()
}

func (nrd *nurseryDevice) stabilizeDevice() {
	nrd.updateStat("startRun", 1)
	if mounted, err := fs.IsMount(filepath.Join(nrd.r.deviceRoot, nrd.dev.Device)); nrd.r.checkMounts && (err != nil || mounted != true) {
		nrd.r.logger.Error("[stabilizeDevice] Drive not mounted", zap.String("Device", nrd.dev.Device))
		return
	}
	c := make(chan Object, 100)
	cancel := make(chan struct{})
	defer close(cancel)
	go nrd.objEngine.GetNurseryObjects(nrd.dev.Device, c, cancel)
	for o := range c {
		nrd.updateStat("checkin", 1)
		func() {
			nrd.r.nurseryConcurrencySem <- struct{}{}
			defer func() {
				<-nrd.r.nurseryConcurrencySem
			}()
			if nrd.validateObj(o) {
				o.Stabilize()
			}
		}()
		select {
		case <-time.After(nurseryObjectSleep):
		case <-nrd.canchan:
			return
		}
	}
	nrd.updateStat("PassComplete", 1)
}

func (nrd *nurseryDevice) stabilizeLoop() {
	for {
		select {
		case <-nrd.canchan:
			return
		default:
			nrd.stabilizeDevice()
			time.Sleep(10 * time.Second)
		}
	}
}

func (nrd *nurseryDevice) cancel() {
	close(nrd.canchan)
}

var newNurseryDevice = func(dev *ring.Device, oring ring.Ring, policy int, r *Replicator, objEngine ObjectEngine) *nurseryDevice {
	return &nurseryDevice{
		r:         r,
		dev:       dev,
		policy:    policy,
		oring:     oring,
		passStart: time.Now(),
		canchan:   make(chan struct{}),
		client:    http.Client{Timeout: 10 * time.Second},
		objEngine: objEngine,
	}
}
