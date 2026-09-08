// Copyright 2022 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package buffer

import (
	"fmt"
	stdsync "sync"

	"github.com/atoonk/packetio/netstack/gvisor/pkg/bits"
	"github.com/atoonk/packetio/netstack/gvisor/pkg/sync"
)

const (
	// This is log2(baseChunkSize). This number is used to calculate which pool
	// to use for a payload size by right shifting the payload size by this
	// number and passing the result to MostSignificantOne64.
	baseChunkSizeLog2 = 6

	// This is the size of the buffers in the first pool. Each subsequent pool
	// creates payloads 2^(pool index) times larger than the first pool's
	// payloads.
	baseChunkSize = 1 << baseChunkSizeLog2 // 64

	// MaxChunkSize is largest payload size that we pool. Payloads larger than
	// this will be allocated from the heap and garbage collected as normal.
	MaxChunkSize = baseChunkSize << (numPools - 1) // 64k

	// The number of chunk pools we have for use.
	numPools = 11
)

// chunkPools is a collection of pools for payloads of different sizes. The
// size of the payloads doubles in each successive pool.
var chunkPools [numPools]sync.Pool

func init() {
	for i := 0; i < numPools; i++ {
		chunkSize := baseChunkSize * (1 << i)
		chunkPools[i].New = func() any {
			return &chunk{
				data: make([]byte, chunkSize),
			}
		}
	}
}

// Precondition: 0 <= size <= maxChunkSize
func getChunkPool(size int) *sync.Pool {
	idx := 0
	if size > baseChunkSize {
		idx = bits.MostSignificantOne64(uint64(size) >> baseChunkSizeLog2)
		if size > 1<<(idx+baseChunkSizeLog2) {
			idx++
		}
	}
	if idx >= numPools {
		panic(fmt.Sprintf("pool for chunk size %d does not exist", size))
	}
	return &chunkPools[idx]
}

// Chunk represents a slice of pooled memory.
//
// +stateify savable
type chunk struct {
	chunkRefs
	data []byte
	// external marks memory the caller owns (see NewViewFromExternal): the
	// chunk is never written through a View and never pooled; release, if
	// set, is called once when the last reference goes.
	external bool
	release  func()
}

// externalChunks pools the chunk structs (not the memory) of external views,
// which come and go once per segment on a transmit path.
var externalChunks = stdsync.Pool{New: func() any { return &chunk{} }}

// newExternalChunk wraps caller-owned memory.
func newExternalChunk(data []byte, release func()) *chunk {
	c := externalChunks.Get().(*chunk)
	c.data, c.release, c.external = data, release, true
	c.InitRefs()
	return c
}

// ZeroOnReuse makes newChunk clear a pooled chunk before handing it out
// again. Every user of a fresh view writes the bytes it exposes before
// anything reads them (a received frame is copied in, headers are encoded
// field by field, GrowTo zeroes explicitly when asked to), so the clear is a
// second pass over every buffer for nothing — at 10 Gbit/s of receive it is
// 1.25 GB/s of memset. Off by default in this fork; set it for a stack that
// wants the old belt and braces.
var ZeroOnReuse = false

func newChunk(size int) *chunk {
	var c *chunk
	if size > MaxChunkSize {
		c = &chunk{
			data: make([]byte, size),
		}
	} else {
		pool := getChunkPool(size)
		c = pool.Get().(*chunk)
		if ZeroOnReuse {
			clear(c.data)
		}
	}
	c.InitRefs()
	return c
}

func (c *chunk) destroy() {
	if c.external {
		r := c.release
		c.release, c.data, c.external = nil, nil, false
		if r != nil {
			r()
		}
		externalChunks.Put(c)
		return
	}
	if len(c.data) > MaxChunkSize {
		c.data = nil
		return
	}
	pool := getChunkPool(len(c.data))
	pool.Put(c)
}

func (c *chunk) DecRef() {
	c.chunkRefs.DecRef(c.destroy)
}

func (c *chunk) Clone() *chunk {
	cpy := newChunk(len(c.data))
	copy(cpy.data, c.data)
	return cpy
}
