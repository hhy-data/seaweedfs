package util

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// 模拟真实的 doHeartbeat select 循环
func TestTickerStarvesDelta(t *testing.T) {
	newVolumesChan := make(chan int, 1024)
	tickChan := time.NewTicker(50 * time.Millisecond) // 模拟 5s ticker（加速）
	defer tickChan.Stop()

	const slowSendDuration = 80 * time.Millisecond // 每次发送 > ticker 间隔
	var deltaCount int32
	var tickCount int32
	var totalVolumes int32 = 50

	// 模拟 MountVolume 生产者：批量 mount
	go func() {
		for i := 0; i < int(totalVolumes); i++ {
			newVolumesChan <- i
			time.Sleep(10 * time.Millisecond) // 每隔一会 mount 一个
		}
	}()

	// 模拟 doHeartbeat select 循环
	done := time.After(2 * time.Second)
	for {
		select {
		case volumeMessage := <-newVolumesChan:
			atomic.AddInt32(&deltaCount, 1)
			// 模拟 delta send（快）
			_ = volumeMessage

		case <-tickChan.C:
			atomic.AddInt32(&tickCount, 1)
			// 模拟 slow CollectHeartbeat + stream.Send
			time.Sleep(slowSendDuration)

		case <-done:
			goto finished
		}
	}

finished:
	d := atomic.LoadInt32(&deltaCount)
	tk := atomic.LoadInt32(&tickCount)
	remaining := len(newVolumesChan)

	t.Logf("=== slow send=%v, ticker=%v ===", slowSendDuration, 50*time.Millisecond)
	t.Logf("delta processed: %d / %d", d, totalVolumes)
	t.Logf("tick processed:  %d", tk)
	t.Logf("remaining in chan: %d", remaining)
	t.Logf("send took %v > ticker %v => tick case hogs the loop", slowSendDuration, 50*time.Millisecond)

	if d < totalVolumes {
		t.Errorf("delta starvation: only %d/%d volumes processed", d, totalVolumes)
	}
}

// 对比：batch drain + 优先检查 delta
func TestPrioritySelect(t *testing.T) {
	newVolumesChan := make(chan int, 1024)
	tickChan := time.NewTicker(50 * time.Millisecond)
	defer tickChan.Stop()

	const slowSendDuration = 80 * time.Millisecond
	var deltaCount int32
	var tickCount int32
	var totalVolumes int32 = 50

	go func() {
		for i := 0; i < int(totalVolumes); i++ {
			newVolumesChan <- i
			time.Sleep(10 * time.Millisecond)
		}
	}()

	done := time.After(2 * time.Second)
	for {
		// 优先检查 delta（non-blocking）
		select {
		case v := <-newVolumesChan:
			atomic.AddInt32(&deltaCount, 1)
			_ = v
			continue // 处理完 delta，回去再优先检查
		default:
		}

		// delta 没有了，才进入正常 select
		select {
		case v := <-newVolumesChan:
			atomic.AddInt32(&deltaCount, 1)
			_ = v
		case <-tickChan.C:
			atomic.AddInt32(&tickCount, 1)
			time.Sleep(slowSendDuration)
		case <-done:
			goto finished
		}
	}

finished:
	d := atomic.LoadInt32(&deltaCount)
	tk := atomic.LoadInt32(&tickCount)

	t.Logf("=== priority select: slow send=%v ===", slowSendDuration)
	t.Logf("delta processed: %d / %d", d, totalVolumes)
	t.Logf("tick processed:  %d", tk)

	if d < totalVolumes {
		t.Errorf("delta starvation: only %d/%d volumes processed", d, totalVolumes)
	}
}

// 完整版：priority select + batch drain
func TestPrioritySelectWithBatchDrain(t *testing.T) {
	newVolumesChan := make(chan int, 1024)
	tickChan := time.NewTicker(50 * time.Millisecond)
	defer tickChan.Stop()

	const slowSendDuration = 80 * time.Millisecond
	var tickCount int32
	var totalVolumes int32 = 50
	var batchSizes []int

	// 批量 mount：瞬间涌入
	go func() {
		for i := 0; i < int(totalVolumes); i++ {
			newVolumesChan <- i
		}
	}()

	done := time.After(2 * time.Second)
	totalDeltaProcessed := 0

	for {
		// 优先检查 delta（non-blocking）
		select {
		case first := <-newVolumesChan:
			batch := []int{first}
			// batch drain
			for {
				select {
				case v := <-newVolumesChan:
					batch = append(batch, v)
				default:
					goto drainDone
				}
			}
		drainDone:
			totalDeltaProcessed += len(batch)
			batchSizes = append(batchSizes, len(batch))
			continue
		default:
		}

		select {
		case first := <-newVolumesChan:
			batch := []int{first}
			for {
				select {
				case v := <-newVolumesChan:
					batch = append(batch, v)
				default:
					goto drainDone2
				}
			}
		drainDone2:
			totalDeltaProcessed += len(batch)
			batchSizes = append(batchSizes, len(batch))
		case <-tickChan.C:
			atomic.AddInt32(&tickCount, 1)
			time.Sleep(slowSendDuration)
		case <-done:
			goto finished
		}
	}

finished:
	tk := atomic.LoadInt32(&tickCount)
	t.Logf("=== priority select + batch drain ===")
	t.Logf("delta: %d / %d processed in %d batches", totalDeltaProcessed, totalVolumes, len(batchSizes))
	t.Logf("batch sizes: %v", batchSizes)
	t.Logf("tick processed: %d", tk)

	if totalDeltaProcessed != int(totalVolumes) {
		t.Errorf("expected %d, got %d", totalVolumes, totalDeltaProcessed)
	}
	fmt.Printf("\nSummary: %d volumes → %d batches (avg %.1f per batch), ticks: %d\n",
		totalDeltaProcessed, len(batchSizes), float64(totalDeltaProcessed)/float64(len(batchSizes)), tk)
}
