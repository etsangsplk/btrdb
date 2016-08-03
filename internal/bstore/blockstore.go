package bstore

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/SoftwareDefinedBuildings/btrdb/internal/bprovider"
	"github.com/SoftwareDefinedBuildings/btrdb/internal/cephprovider"
	"github.com/SoftwareDefinedBuildings/btrdb/internal/configprovider"
	"github.com/SoftwareDefinedBuildings/btrdb/internal/fileprovider"
	"github.com/pborman/uuid"
)

const LatestGeneration = uint64(^(uint64(0)))

func UUIDToMapKey(id uuid.UUID) [16]byte {
	rv := [16]byte{}
	copy(rv[:], id)
	return rv
}

type BlockStore struct {
	_wlocks map[[16]byte]*sync.Mutex
	glock   sync.RWMutex

	cachemap map[uint64]*CacheItem
	cacheold *CacheItem
	cachenew *CacheItem
	cachemtx sync.Mutex
	cachelen uint64
	cachemax uint64

	cachemiss uint64
	cachehit  uint64

	store bprovider.StorageProvider
	alloc chan uint64
}

var block_buf_pool = sync.Pool{
	New: func() interface{} {
		return make([]byte, DBSIZE+5)
	},
}

var ErrDatablockNotFound = errors.New("Coreblock not found")
var ErrGenerationNotFound = errors.New("Generation not found")

/* A generation stores all the information acquired during a write pass.
 * A superblock contains all the information required to navigate a tree.
 */
type Generation struct {
	Cur_SB     *Superblock
	New_SB     *Superblock
	cblocks    []*Coreblock
	vblocks    []*Vectorblock
	blockstore *BlockStore
	flushed    bool
}

func (g *Generation) UpdateRootAddr(addr uint64) {
	g.New_SB.root = addr
}
func (g *Generation) Uuid() *uuid.UUID {
	return &g.Cur_SB.uuid
}

func (g *Generation) Number() uint64 {
	return g.New_SB.gen
}

// func (bs *BlockStore) UnlinkGenerations(id uuid.UUID, sgen uint64, egen uint64) error {
// 	iter := bs.db.C("superblocks").Find(bson.M{"uuid": id.String(), "gen": bson.M{"$gte": sgen, "$lt": egen}, "unlinked": false}).Iter()
// 	rs := fake_sblock{}
// 	for iter.Next(&rs) {
// 		rs.Unlinked = true
// 		_, err := bs.db.C("superblocks").Upsert(bson.M{"uuid": id.String(), "gen": rs.Gen}, rs)
// 		if err != nil {
// 			lg.Panic(err)
// 		}
// 	}
// 	return nil
// }
func NewBlockStore(cfg configprovider.Configuration) (*BlockStore, error) {
	bs := BlockStore{}
	bs._wlocks = make(map[[16]byte]*sync.Mutex)

	bs.alloc = make(chan uint64, 256)
	go func() {
		relocation_addr := uint64(RELOCATION_BASE)
		for {
			bs.alloc <- relocation_addr
			relocation_addr += 1
			if relocation_addr < RELOCATION_BASE {
				relocation_addr = RELOCATION_BASE
			}
		}
	}()

	if cfg.ClusterEnabled() {
		bs.store = new(cephprovider.CephStorageProvider)
	} else {
		bs.store = new(fileprovider.FileStorageProvider)
	}

	bs.store.Initialize(cfg)
	cachesz := cfg.BlockCache()
	bs.initCache(uint64(cachesz))

	return &bs, nil
}

/*
 * This obtains a generation, blocking if necessary
 */
func (bs *BlockStore) ObtainGeneration(id uuid.UUID) *Generation {
	//The first thing we do is obtain a write lock on the UUID, as a generation
	//represents a lock
	mk := UUIDToMapKey(id)
	bs.glock.Lock()
	mtx, ok := bs._wlocks[mk]
	if !ok {
		//Mutex doesn't exist so is unlocked
		mtx = new(sync.Mutex)
		mtx.Lock()
		bs._wlocks[mk] = mtx
	} else {
		mtx.Lock()
	}
	bs.glock.Unlock()

	gen := &Generation{
		cblocks: make([]*Coreblock, 0, 8192),
		vblocks: make([]*Vectorblock, 0, 8192),
	}
	//We need a generation. Lets see if one is on disk
	existingVer := bs.store.GetStreamVersion(id[:])
	if existingVer == 0 {
		//Ok just create a new superblock/generation
		gen.Cur_SB = NewSuperblock(id)
	} else {
		//Ok the sblock exists, lets load it
		sbarr := make([]byte, SUPERBLOCK_SIZE)
		sbarr = bs.store.ReadSuperBlock(id[:], existingVer, sbarr)
		if sbarr == nil {
			lg.Panicf("Your database is corrupt, superblock %d on stream %s does not exist", existingVer, id.String())
		}
		gen.Cur_SB = DeserializeSuperblock(id, existingVer, sbarr)
	}

	gen.New_SB = gen.Cur_SB.CloneInc()
	gen.blockstore = bs
	return gen
}

//The returned address map is primarily for unit testing
func (gen *Generation) Commit() (map[uint64]uint64, error) {
	if gen.flushed {
		return nil, errors.New("Already Flushed")
	}

	then := time.Now()
	address_map := LinkAndStore([]byte(*gen.Uuid()), gen.blockstore, gen.blockstore.store, gen.vblocks, gen.cblocks)
	rootaddr, ok := address_map[gen.New_SB.root]
	if !ok {
		lg.Panic("Could not obtain root address")
	}
	gen.New_SB.root = rootaddr
	dt := time.Now().Sub(then)
	_ = dt
	//lg.Infof("rawlp[%s %s=%d,%s=%d,%s=%d]", "las", "latus", uint64(dt/time.Microsecond), "cblocks", len(gen.cblocks), "vblocks", len(gen.vblocks))
	//log.Info("(LAS %4dus %dc%dv) ins blk u=%v gen=%v root=0x%016x",
	//	uint64(dt/time.Microsecond), len(gen.cblocks), len(gen.vblocks), gen.Uuid().String(), gen.Number(), rootaddr)
	/*if len(gen.vblocks) > 100 {
		total := 0
		for _, v:= range gen.vblocks {
			total += int(v.Len)
		}
		log.Critical("Triggered vblock examination: %v blocks, %v points, %v avg", len(gen.vblocks), total, total/len(gen.vblocks))
	}*/
	gen.vblocks = nil
	gen.cblocks = nil

	gen.blockstore.store.WriteSuperBlock(gen.New_SB.uuid, gen.New_SB.gen, gen.New_SB.Serialize())
	gen.blockstore.store.SetStreamVersion(gen.New_SB.uuid, gen.New_SB.gen)
	gen.flushed = true
	gen.blockstore.glock.RLock()
	gen.blockstore._wlocks[UUIDToMapKey(*gen.Uuid())].Unlock()
	gen.blockstore.glock.RUnlock()
	return address_map, nil
}

func (bs *BlockStore) allocateBlock() uint64 {
	relocation_address := <-bs.alloc
	return relocation_address
}

/**
 * The real function is supposed to allocate an address for the data
 * block, reserving it on disk, and then give back the data block that
 * can be filled in
 * This stub makes up an address, and mongo pretends its real
 */
func (gen *Generation) AllocateCoreblock() (*Coreblock, error) {
	cblock := &Coreblock{}
	cblock.Identifier = gen.blockstore.allocateBlock()
	cblock.Generation = gen.Number()
	gen.cblocks = append(gen.cblocks, cblock)
	return cblock, nil
}

func (gen *Generation) AllocateVectorblock() (*Vectorblock, error) {
	vblock := &Vectorblock{}
	vblock.Identifier = gen.blockstore.allocateBlock()
	vblock.Generation = gen.Number()
	gen.vblocks = append(gen.vblocks, vblock)
	return vblock, nil
}

func (bs *BlockStore) FreeCoreblock(cb **Coreblock) {
	*cb = nil
}

func (bs *BlockStore) FreeVectorblock(vb **Vectorblock) {
	*vb = nil
}

func (bs *BlockStore) ReadDatablock(uuid uuid.UUID, addr uint64, impl_Generation uint64, impl_Pointwidth uint8, impl_StartTime int64) Datablock {
	//Try hit the cache first
	db := bs.cacheGet(addr)
	if db != nil {
		return db
	}
	syncbuf := block_buf_pool.Get().([]byte)
	trimbuf := bs.store.Read([]byte(uuid), addr, syncbuf)
	switch DatablockGetBufferType(trimbuf) {
	case Core:
		rv := &Coreblock{}
		rv.Deserialize(trimbuf)
		block_buf_pool.Put(syncbuf)
		rv.Identifier = addr
		rv.Generation = impl_Generation
		rv.PointWidth = impl_Pointwidth
		rv.StartTime = impl_StartTime
		bs.cachePut(addr, rv)
		return rv
	case Vector:
		rv := &Vectorblock{}
		rv.Deserialize(trimbuf)
		block_buf_pool.Put(syncbuf)
		rv.Identifier = addr
		rv.Generation = impl_Generation
		rv.PointWidth = impl_Pointwidth
		rv.StartTime = impl_StartTime
		bs.cachePut(addr, rv)
		return rv
	}
	lg.Panic("Strange datablock type")
	return nil
}

type fake_sblock struct {
	Uuid     string
	Gen      uint64
	Root     uint64
	Unlinked bool
}

func (bs *BlockStore) LoadSuperblock(id uuid.UUID, generation uint64) *Superblock {
	latestGen := bs.store.GetStreamVersion(id)
	if latestGen == 0 {
		return nil
	}
	if generation == LatestGeneration {
		generation = latestGen
	}
	if generation > latestGen {
		return nil
	}
	buff := make([]byte, 16)
	sbarr := bs.store.ReadSuperBlock(id, generation, buff)
	if sbarr == nil {
		lg.Panicf("Your database is corrupt, superblock %d for stream %s should exist (but doesn't)", generation, id.String())
	}
	sb := DeserializeSuperblock(id, generation, sbarr)
	return sb
}

func CreateDatabase(cfg configprovider.Configuration) {
	if cfg.ClusterEnabled() {
		cp := new(cephprovider.CephStorageProvider)
		err := cp.CreateDatabase(cfg)
		if err != nil {
			lg.Critical("Error on create: %v", err)
			os.Exit(1)
		}
	} else {
		if err := os.MkdirAll(cfg.StorageFilepath(), 0755); err != nil {
			lg.Panic(err)
		}
		fp := new(fileprovider.FileStorageProvider)
		err := fp.CreateDatabase(cfg)
		if err != nil {
			lg.Critical("Error on create: %v", err)
			os.Exit(1)
		}
	}
}
