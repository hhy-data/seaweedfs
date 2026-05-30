package weed_server

import (
	"fmt"
	"os"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/operation"

	"google.golang.org/grpc"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/security"
	"github.com/seaweedfs/seaweedfs/weed/storage/backend"

	"golang.org/x/net/context"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func (vs *VolumeServer) GetMaster(ctx context.Context) pb.ServerAddress {
	return vs.currentMaster
}

func (vs *VolumeServer) checkWithMaster() (err error) {
	for {
		for _, master := range vs.SeedMasterNodes {
			err = operation.WithMasterServerClient(false, master, vs.grpcDialOption, func(masterClient master_pb.SeaweedClient) error {
				resp, err := masterClient.GetMasterConfiguration(context.Background(), &master_pb.GetMasterConfigurationRequest{})
				if err != nil {
					return fmt.Errorf("get master %s configuration: %v", master, err)
				}
				vs.metricsAddress, vs.metricsIntervalSec = resp.MetricsAddress, int(resp.MetricsIntervalSeconds)
				backend.LoadFromPbStorageBackends(resp.StorageBackends)
				return nil
			})
			if err == nil {
				return
			} else {
				glog.V(0).Infof("checkWithMaster %s: %v", master, err)
			}
		}
		time.Sleep(1790 * time.Millisecond)
	}
}

func (vs *VolumeServer) heartbeat() {

	glog.V(0).Infof("Volume server start with seed master nodes: %v", vs.SeedMasterNodes)
	vs.store.SetDataCenter(vs.dataCenter)
	vs.store.SetRack(vs.rack)

	grpcDialOption := security.LoadClientTLS(util.GetViper(), "grpc.volume")

	var err error
	var newLeader pb.ServerAddress
	for vs.isHeartbeating {
		for _, master := range vs.SeedMasterNodes {
			if newLeader != "" {
				// the new leader may actually is the same master
				// need to wait a bit before adding itself
				time.Sleep(3 * time.Second)
				master = newLeader
			}
			vs.store.MasterAddress = master
			newLeader, err = vs.doHeartbeat(master, grpcDialOption, time.Duration(vs.pulseSeconds)*time.Second)
			if err != nil {
				glog.V(0).Infof("heartbeat to %s error: %v", master, err)
				time.Sleep(time.Duration(vs.pulseSeconds) * time.Second)
				newLeader = ""
				vs.store.MasterAddress = ""
			}
			if !vs.isHeartbeating {
				break
			}
		}
	}
}

func (vs *VolumeServer) StopHeartbeat() (isAlreadyStopping bool) {
	if !vs.isHeartbeating {
		return true
	}
	vs.isHeartbeating = false
	close(vs.stopChan)
	return false
}

func (vs *VolumeServer) doHeartbeat(masterAddress pb.ServerAddress, grpcDialOption grpc.DialOption, sleepInterval time.Duration) (newLeader pb.ServerAddress, err error) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	grpcConnection, err := pb.GrpcDial(ctx, masterAddress.ToGrpcAddress(), false, grpcDialOption)
	if err != nil {
		return "", fmt.Errorf("fail to dial %s : %v", masterAddress, err)
	}
	defer grpcConnection.Close()

	client := master_pb.NewSeaweedClient(grpcConnection)
	stream, err := client.SendHeartbeat(ctx)
	if err != nil {
		glog.V(0).Infof("SendHeartbeat to %s: %v", masterAddress, err)
		return "", err
	}
	glog.V(0).Infof("Heartbeat to: %v", masterAddress)
	vs.currentMaster = masterAddress

	doneChan := make(chan error, 1)

	go func() {
		for {
			in, err := stream.Recv()
			if err != nil {
				doneChan <- err
				return
			}
			if len(in.DuplicatedUuids) > 0 {
				var duplicateDir []string
				for _, loc := range vs.store.Locations {
					for _, uuid := range in.DuplicatedUuids {
						if uuid == loc.DirectoryUuid {
							duplicateDir = append(duplicateDir, loc.Directory)
						}
					}
				}
				glog.Errorf("Shut down Volume Server due to duplicate volume directories: %v", duplicateDir)
				os.Exit(1)
			}
			volumeOptsChanged := false
			if vs.store.GetPreallocate() != in.GetPreallocate() {
				vs.store.SetPreallocate(in.GetPreallocate())
				volumeOptsChanged = true
			}
			if in.GetVolumeSizeLimit() != 0 && vs.store.GetVolumeSizeLimit() != in.GetVolumeSizeLimit() {
				vs.store.SetVolumeSizeLimit(in.GetVolumeSizeLimit())
				volumeOptsChanged = true
			}
			if volumeOptsChanged {
				if vs.store.MaybeAdjustVolumeMax() {
					if err = stream.Send(vs.store.CollectHeartbeat()); err != nil {
						glog.V(0).Infof("Volume Server Failed to talk with master %s: %v", vs.currentMaster, err)
						return
					}
				}
			}
			if in.GetLeader() != "" && string(vs.currentMaster) != in.GetLeader() {
				glog.V(0).Infof("Volume Server found a new master newLeader: %v instead of %v", in.GetLeader(), vs.currentMaster)
				newLeader = pb.ServerAddress(in.GetLeader())
				doneChan <- nil
				return
			}
		}
	}()

	if err = stream.Send(vs.store.CollectHeartbeat()); err != nil {
		glog.V(0).Infof("Volume Server Failed to talk with master %s: %v", masterAddress, err)
		return "", err
	}

	if err = stream.Send(vs.store.CollectErasureCodingHeartbeat()); err != nil {
		glog.V(0).Infof("Volume Server Failed to talk with master %s: %v", masterAddress, err)
		return "", err
	}

	volumeTickChan := time.NewTicker(sleepInterval)
	defer volumeTickChan.Stop()
	ecShardTickChan := time.NewTicker(17 * sleepInterval)
	defer ecShardTickChan.Stop()
	dataCenter := vs.store.GetDataCenter()
	rack := vs.store.GetRack()
	ip := vs.store.Ip
	port := uint32(vs.store.Port)
	for {
		select {
		case first := <-vs.store.NewVolumesChan:
			volumes := util.ToPointers(util.DrainChannel(vs.store.NewVolumesChan, first))
			deltaBeat := &master_pb.Heartbeat{
				Ip:         ip,
				Port:       port,
				DataCenter: dataCenter,
				Rack:       rack,
				NewVolumes: volumes,
			}
			glog.V(0).Infof("volume server %s:%d adds %d volumes", vs.store.Ip, vs.store.Port, len(volumes))
			if err = stream.Send(deltaBeat); err != nil {
				glog.V(0).Infof("Volume Server Failed to update to master %s: %v", masterAddress, err)
				return "", err
			}
		case first := <-vs.store.NewEcShardsChan:
			shards := util.ToPointers(util.DrainChannel(vs.store.NewEcShardsChan, first))
			deltaBeat := &master_pb.Heartbeat{
				Ip:           ip,
				Port:         port,
				DataCenter:   dataCenter,
				Rack:         rack,
				NewEcShards: shards,
			}
			glog.V(0).Infof("volume server %s:%d adds %d ec shards", vs.store.Ip, vs.store.Port, len(shards))
			if err = stream.Send(deltaBeat); err != nil {
				glog.V(0).Infof("Volume Server Failed to update to master %s: %v", masterAddress, err)
				return "", err
			}
		case first := <-vs.store.DeletedVolumesChan:
			volumes := util.ToPointers(util.DrainChannel(vs.store.DeletedVolumesChan, first))
			deltaBeat := &master_pb.Heartbeat{
				Ip:             ip,
				Port:           port,
				DataCenter:     dataCenter,
				Rack:           rack,
				DeletedVolumes: volumes,
			}
			glog.V(0).Infof("volume server %s:%d deletes %d volumes", vs.store.Ip, vs.store.Port, len(volumes))
			if err = stream.Send(deltaBeat); err != nil {
				glog.V(0).Infof("Volume Server Failed to update to master %s: %v", masterAddress, err)
				return "", err
			}
		case first := <-vs.store.DeletedEcShardsChan:
			shards := util.ToPointers(util.DrainChannel(vs.store.DeletedEcShardsChan, first))
			deltaBeat := &master_pb.Heartbeat{
				Ip:               ip,
				Port:             port,
				DataCenter:       dataCenter,
				Rack:             rack,
				DeletedEcShards: shards,
			}
			glog.V(0).Infof("volume server %s:%d deletes %d ec shards", vs.store.Ip, vs.store.Port, len(shards))
			if err = stream.Send(deltaBeat); err != nil {
				glog.V(0).Infof("Volume Server Failed to update to master %s: %v", masterAddress, err)
				return "", err
			}
		case <-volumeTickChan.C:
			glog.V(4).Infof("volume server %s:%d heartbeat", vs.store.Ip, vs.store.Port)
			vs.store.MaybeAdjustVolumeMax()
			if err = stream.Send(vs.store.CollectHeartbeat()); err != nil {
				glog.V(0).Infof("Volume Server Failed to talk with master %s: %v", masterAddress, err)
				return "", err
			}
		case <-ecShardTickChan.C:
			glog.V(4).Infof("volume server %s:%d ec heartbeat", vs.store.Ip, vs.store.Port)
			if err = stream.Send(vs.store.CollectErasureCodingHeartbeat()); err != nil {
				glog.V(0).Infof("Volume Server Failed to talk with master %s: %v", masterAddress, err)
				return "", err
			}
		case err = <-doneChan:
			return
		case <-vs.stopChan:
			var volumeMessages []*master_pb.VolumeInformationMessage
			emptyBeat := &master_pb.Heartbeat{
				Ip:           ip,
				Port:         port,
				PublicUrl:    vs.store.PublicUrl,
				MaxFileKey:   uint64(0),
				DataCenter:   dataCenter,
				Rack:         rack,
				Volumes:      volumeMessages,
				HasNoVolumes: len(volumeMessages) == 0,
			}
			glog.V(1).Infof("volume server %s:%d stops and deletes all volumes", vs.store.Ip, vs.store.Port)
			if err = stream.Send(emptyBeat); err != nil {
				glog.V(0).Infof("Volume Server Failed to update to master %s: %v", masterAddress, err)
				return "", err
			}
			return
		}
	}
}
