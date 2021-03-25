// Copyright 2021 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package filetransfer

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/mendersoftware/mender-connect/config"
	"github.com/mendersoftware/mender-connect/session/model"
	"github.com/mendersoftware/mender-connect/utils"
)

const (
	S_ISUID = 0004000
)

var (
	ErrChrootViolation          = errors.New("the target file path is outside chroot")
	ErrFileOwnerMismatch        = errors.New("the file owner does not match")
	ErrFileGroupMismatch        = errors.New("the file group does not match")
	ErrFollowLinksForbidden     = errors.New("forbidden to follow the link")
	ErrForbiddenToOverwriteFile = errors.New("forbidden to overwrite the file")
	ErrFileTooBig               = errors.New("the file size is over the limit")
	ErrSuidModeForbidden        = errors.New("the set uid mode is forbidden")
	ErrTxBytesLimitExhausted    = errors.New("transmitted bytes limit exhausted")
	ErrOnlyRegularFilesAllowed  = errors.New("only regular files are allowed")
)

var (
	countersUpdateSleepTimeS = 5
)

type Counters struct {
	bytesTransferred           uint64
	bytesReceived              uint64
	bytesTransferred50         uint64
	bytesReceived50            uint64
	bytesTransferred51         uint64
	bytesReceived51            uint64
	bytesTransferredLastH      uint64
	bytesReceivedLastH         uint64
	currentTxRate              float64
	currentRxRate              float64
	currentTxRateW             float64
	currentRxRateW             float64
	bytesTransferredLast1W     float64
	bytesTransferredLast5W     float64
	bytesTransferredLast15W    float64
	bytesReceivedLast1W        float64
	bytesReceivedLast5W        float64
	bytesReceivedLast15W       float64
	bytesTransferredLastUpdate time.Time
	bytesReceivedLastUpdate    time.Time
	period                     uint64
}

type Permit struct {
	limits   config.Limits
	counters Counters
	// mutex to protect the writes and reads of the counters
	countersMutex *sync.Mutex
}

var countersMutex = &sync.Mutex{}
var deviceCountersLastH = Counters{
	bytesTransferred:           0,
	bytesReceived:              0,
	bytesTransferred50:         0,
	bytesReceived50:            0,
	bytesTransferred51:         0,
	bytesReceived51:            0,
	bytesTransferredLastUpdate: time.Now(),
	bytesReceivedLastUpdate:    time.Now(),
	period:                     0,
}
var counterUpdateRunning = false
var counterUpdateStarted = make(chan bool, 1)

func NewPermit(config config.Limits) *Permit {
	countersMutex.Lock()
	defer countersMutex.Unlock()
	go updatePerHourCounters()
	<-counterUpdateStarted
	return &Permit{
		limits: config,
		counters: Counters{
			bytesTransferred:           0,
			bytesReceived:              0,
			bytesTransferredLastUpdate: time.Now().UTC(),
			bytesReceivedLastUpdate:    time.Now().UTC(),
		},
		// mutex to protect the writes and reads of the Counters
		countersMutex: &sync.Mutex{},
	}
}

func (p *Permit) UploadFile(fileStat model.FileInfo) error {
	if !p.limits.Enabled {
		return nil
	}

	filePath := *fileStat.Path

	//this one actually does nothing, since at the moment of writing,
	//InitFileUpload does not get the size of the file upfront,
	//so this potentially can work once the remote sends the size
	if p.limits.FileTransfer.MaxFileSize > 0 &&
		fileStat.Size != nil &&
		uint64(*fileStat.Size) > p.limits.FileTransfer.MaxFileSize {
		return ErrFileTooBig
	}

	if !utils.IsInChroot(filePath, p.limits.FileTransfer.Chroot) {
		return ErrChrootViolation
	}

	if !p.limits.FileTransfer.FollowSymLinks {
		absolutePath, err := filepath.EvalSymlinks(path.Dir(filePath))
		if err != nil {
			return err
		} else {
			if absolutePath != path.Dir(filePath) {
				return ErrFollowLinksForbidden
			}
		}
	}

	if !p.limits.FileTransfer.AllowOverwrite && utils.FileExists(filePath) {
		return ErrForbiddenToOverwriteFile
	}

	if p.limits.FileTransfer.AllowOverwrite && utils.FileExists(filePath) {
		if !utils.FileOwnerMatches(filePath, p.limits.FileTransfer.OwnerPut) {
			return ErrFileOwnerMismatch
		}

		if !utils.FileGroupMatches(filePath, p.limits.FileTransfer.GroupPut) {
			return ErrFileGroupMismatch
		}
	}

	if !p.limits.FileTransfer.AllowSuid &&
		fileStat.Mode != nil &&
		(os.FileMode(*fileStat.Mode)&os.ModeSetuid) != 0 {
		return ErrSuidModeForbidden
	}

	return nil
}

func (p *Permit) DownloadFile(fileStat model.FileInfo) error {
	if !p.limits.Enabled {
		return nil
	}

	filePath := *fileStat.Path

	if p.limits.FileTransfer.RegularFilesOnly && !utils.IsRegularFile(filePath) {
		return ErrOnlyRegularFilesAllowed
	}

	if !utils.IsInChroot(filePath, p.limits.FileTransfer.Chroot) {
		return ErrChrootViolation
	}

	if !utils.FileOwnerMatches(filePath, p.limits.FileTransfer.OwnerGet) {
		return ErrFileOwnerMismatch
	}

	if !utils.FileGroupMatches(filePath, p.limits.FileTransfer.GroupGet) {
		return ErrFileGroupMismatch
	}

	if !p.limits.FileTransfer.FollowSymLinks {
		absolutePath, err := filepath.EvalSymlinks(filePath)
		if err != nil {
			return err
		} else {
			if absolutePath != filePath {
				return ErrFollowLinksForbidden
			}
		}
	}

	if p.limits.FileTransfer.MaxFileSize > 0 {
		fileSize := utils.FileSize(filePath)
		if fileSize > 0 && p.limits.FileTransfer.MaxFileSize < uint64(fileSize) {
			return ErrFileTooBig
		}
	}

	return nil
}

func (p *Permit) BytesSent(n uint64) (belowLimit bool) {
	if !p.limits.Enabled {
		return true
	}

	countersMutex.Lock()
	defer countersMutex.Unlock()

	belowLimit = true
	if n != 0 {
		if deviceCountersLastH.bytesTransferred < math.MaxUint64-n {
			deviceCountersLastH.bytesTransferred += n
		}
	}
	if p.limits.FileTransfer.Counters.MaxBytesTxPerHour > 0 &&
		deviceCountersLastH.bytesTransferred >= p.limits.FileTransfer.Counters.MaxBytesTxPerHour {
		belowLimit = false
	}

	p.countersMutex.Lock()
	defer p.countersMutex.Unlock()
	if n != 0 {
		if p.counters.bytesTransferred < math.MaxUint64-n {
			p.counters.bytesTransferred += n
		}
	}
	return belowLimit
}

func (p *Permit) BytesReceived(n uint64) (belowLimit bool) {
	if !p.limits.Enabled {
		return true
	}

	countersMutex.Lock()
	defer countersMutex.Unlock()

	belowLimit = true
	if n != 0 {
		if deviceCountersLastH.bytesReceived < math.MaxUint64-n {
			deviceCountersLastH.bytesReceived += n
		}
	}
	if p.limits.FileTransfer.Counters.MaxBytesRxPerHour > 0 &&
		deviceCountersLastH.bytesReceived >= p.limits.FileTransfer.Counters.MaxBytesRxPerHour {
		belowLimit = false
	}

	p.countersMutex.Lock()
	defer p.countersMutex.Unlock()
	if n != 0 {
		if p.counters.bytesReceived < math.MaxUint64-n {
			p.counters.bytesReceived += n
		}
	}
	return belowLimit
}

func (p *Permit) BelowMaxAllowedFileSize(offset int64) (belowLimit bool) {
	if !p.limits.Enabled {
		return true
	}

	if offset < 0 {
		return true
	}
	if p.limits.FileTransfer.MaxFileSize > 0 &&
		uint64(offset) >= p.limits.FileTransfer.MaxFileSize {
		return false
	} else {
		return true
	}
}

func (p *Permit) PreserveModes(path string, mode os.FileMode) error {
	if !p.limits.Enabled {
		return nil
	}

	if (mode & S_ISUID) != 0 {
		mode &= os.ModePerm
		if p.limits.FileTransfer.Umask != "" {
			umask, err := strconv.ParseUint(p.limits.FileTransfer.Umask, 8, 32)
			if err != nil {
				return err
			}

			mode = os.ModePerm ^ os.FileMode(uint32(os.ModePerm)&uint32(umask))
		}
		mode |= os.ModeSetuid
	} else {
		if p.limits.FileTransfer.Umask != "" {
			umask, err := strconv.ParseUint(p.limits.FileTransfer.Umask, 8, 32)
			if err != nil {
				return err
			}

			mode = os.ModePerm ^ os.FileMode(uint32(os.ModePerm)&uint32(umask))
		}
		mode &= os.ModePerm
	}

	if !p.limits.FileTransfer.DoNotPreserveMode {
		return os.Chmod(path, mode)
	} else {
		return nil
	}
}

func (p *Permit) PreserveOwnerGroup(path string, uid int, gid int) error {
	if !p.limits.Enabled {
		return nil
	}

	forcedSet := false
	if p.limits.FileTransfer.OwnerPut != "" {
		u, err := user.Lookup(p.limits.FileTransfer.OwnerPut)
		if err != nil {
			return err
		}
		uid, err = strconv.Atoi(u.Uid)
		if err != nil {
			return err
		}
		forcedSet = true
	}
	if p.limits.FileTransfer.GroupPut != "" {
		u, err := user.LookupGroup(p.limits.FileTransfer.GroupPut)
		if err != nil {
			return err
		}
		gid, err = strconv.Atoi(u.Gid)
		if err != nil {
			return err
		}
		forcedSet = true
	}
	if forcedSet {
		return os.Chown(path, uid, gid)
	}
	if !p.limits.FileTransfer.DoNotPreserveOwner {
		return os.Chown(path, uid, gid)
	} else {
		return nil
	}
}

func updatePerHourCounters() {
	if counterUpdateRunning {
		counterUpdateStarted <- false
		return
	}

	counterUpdateRunning = true
	counterUpdateStarted <- true
	expWeight1m := math.Exp(-float64(countersUpdateSleepTimeS) * 0.016666666666666666)  // / 60.0)
	expWeight5m := math.Exp(-float64(countersUpdateSleepTimeS) * 0.003333333333333333)  // / (5*60.0))
	expWeight15m := math.Exp(-float64(countersUpdateSleepTimeS) * 0.001111111111111111) // / (15*60.0))
	deviceCountersLastH.bytesReceived50 = 0
	deviceCountersLastH.bytesTransferred50 = 0
	deviceCountersLastH.bytesReceived51 = 0
	deviceCountersLastH.bytesTransferred51 = 0
	deviceCountersLastH.bytesReceivedLast1W=0.0
	deviceCountersLastH.bytesReceivedLast5W=0.0
	deviceCountersLastH.bytesReceivedLast15W=0.0
	deviceCountersLastH.bytesTransferredLast1W=0.0
	deviceCountersLastH.bytesTransferredLast5W=0.0
	deviceCountersLastH.bytesTransferredLast15W=0.0
	for counterUpdateRunning {
		//for minute := 0; minute < 60; minute++ {
		bytesReceived50 := deviceCountersLastH.bytesReceived
		bytesTransferred50 := deviceCountersLastH.bytesTransferred
		time.Sleep(time.Duration(countersUpdateSleepTimeS) * time.Second)
		bytesReceived51 := deviceCountersLastH.bytesReceived
		bytesTransferred51 := deviceCountersLastH.bytesTransferred

		countersMutex.Lock()
		dBytes := float64(bytesReceived51 - bytesReceived50)
		deviceCountersLastH.bytesReceivedLast1W = expWeight1m*deviceCountersLastH.bytesReceivedLast1W +
			dBytes - expWeight1m*dBytes
		deviceCountersLastH.bytesReceivedLast5W = expWeight5m*deviceCountersLastH.bytesReceivedLast5W +
			dBytes - expWeight5m*dBytes
		deviceCountersLastH.bytesReceivedLast15W = expWeight15m*deviceCountersLastH.bytesReceivedLast15W +
			dBytes - expWeight15m*dBytes
		fmt.Fprintf(os.Stderr, "\nperiod:%d dBytes:%.2f rx 1,5,15m: %.2f,%.2f,%.2f",
			deviceCountersLastH.period, dBytes, deviceCountersLastH.bytesReceivedLast1W, deviceCountersLastH.bytesReceivedLast5W, deviceCountersLastH.bytesReceivedLast15W)
		dBytes = float64(bytesTransferred51 - bytesTransferred50)
		deviceCountersLastH.bytesTransferredLast1W = expWeight1m*deviceCountersLastH.bytesTransferredLast1W +
			dBytes - expWeight1m*dBytes
		deviceCountersLastH.bytesTransferredLast5W = expWeight5m*deviceCountersLastH.bytesTransferredLast5W +
			dBytes - expWeight5m*dBytes
		deviceCountersLastH.bytesTransferredLast15W = expWeight15m*deviceCountersLastH.bytesTransferredLast15W +
			dBytes - expWeight15m*dBytes

		rate := float64(deviceCountersLastH.bytesTransferred51-deviceCountersLastH.bytesTransferred50) * 0.2
		deviceCountersLastH.currentTxRateW = expWeight1m*deviceCountersLastH.currentTxRateW +
			rate - expWeight1m*rate
		rate = float64(deviceCountersLastH.bytesReceived51-deviceCountersLastH.bytesReceived50) * 0.2
		deviceCountersLastH.currentRxRateW = expWeight1m*deviceCountersLastH.currentRxRateW +
			rate - expWeight1m*rate
		if deviceCountersLastH.period >= math.MaxUint32-1 {
			deviceCountersLastH.period = 0
		}
		deviceCountersLastH.period++
		sinceLastUpdateS := time.Now().Unix() - deviceCountersLastH.bytesTransferredLastUpdate.Unix()
		if deviceCountersLastH.bytesTransferred != 0 {
			deviceCountersLastH.currentTxRate = float64(deviceCountersLastH.bytesTransferred*1.0) / float64(sinceLastUpdateS)
		}
		sinceLastUpdateS = time.Now().Unix() - deviceCountersLastH.bytesReceivedLastUpdate.Unix()
		if deviceCountersLastH.bytesReceived != 0 {
			deviceCountersLastH.currentRxRate = float64(deviceCountersLastH.bytesReceived*1.0) / float64(sinceLastUpdateS)
		}
		countersMutex.Unlock()
		//}
		//countersMutex.Lock()
		//deviceCountersLastH.bytesTransferredLastH = deviceCountersLastH.bytesTransferred
		//deviceCountersLastH.bytesReceivedLastH = deviceCountersLastH.bytesTransferred
		//deviceCountersLastH.bytesTransferred = 0
		//deviceCountersLastH.bytesReceived = 0
		//deviceCountersLastH.currentRxRate = 0.0
		//deviceCountersLastH.currentTxRate = 0.0
		//countersMutex.Unlock()
	}
}

func GetCounters() (uint64, uint64, float64, float64, float64, float64, float64, float64, float64, float64, float64, float64) {
	countersMutex.Lock()
	defer countersMutex.Unlock()

	return deviceCountersLastH.bytesTransferred,
		deviceCountersLastH.bytesReceived,
		deviceCountersLastH.currentTxRate,
		deviceCountersLastH.currentRxRate,
		deviceCountersLastH.currentTxRateW,
		deviceCountersLastH.currentRxRateW,
		deviceCountersLastH.bytesTransferredLast1W,
		deviceCountersLastH.bytesTransferredLast5W,
		deviceCountersLastH.bytesTransferredLast15W,
		deviceCountersLastH.bytesReceivedLast1W,
		deviceCountersLastH.bytesReceivedLast5W,
		deviceCountersLastH.bytesReceivedLast15W
}
