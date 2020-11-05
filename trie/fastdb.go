package trie

import (
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
)

type tValue struct {
	value   []byte
	deleted bool
}

type FastDB struct {
	db         *Database
	cache      map[string]tValue
	cachedHash common.Hash
}

func (f *FastDB) DDD() string {
	ss := fmt.Sprintf("len=%v", len(f.cache))
	for k, v := range f.cache {
		ss += fmt.Sprintf("k:%v v:%v", k, v)
	}
	return ss
}
func NewFastDB(db *Database) *FastDB {
	return &FastDB{
		db:    db,
		cache: make(map[string]tValue),
	}
}

func (f *FastDB) cacheCopy() map[string]tValue {
	ans := make(map[string]tValue, 0)
	for k, v := range f.cache {
		ans[k] = v
	}
	return ans
}
func (f *FastDB) Copy() *FastDB {
	return &FastDB{
		db:    f.db,
		cache: f.cacheCopy(),
	}
}

func (f *FastDB) GetKey(key []byte) []byte {
	panic("no need to implement")
}
func (f *FastDB) TryGet(key []byte) ([]byte, error) {
	if data, ok := f.cache[string(key)]; ok && !data.deleted {
		return data.value, nil
	}
	data, _ := f.db.diskdb.Get(key)
	return data, nil
}
func (f *FastDB) TryUpdate(key, value []byte) error {
	//fmt.Println("600000000000000", hex.EncodeToString(key), hex.EncodeToString(value))
	f.cache[string(key)] = tValue{
		value:   value,
		deleted: false,
	}
	return nil
}
func (f *FastDB) TryDelete(key []byte) error {
	f.cache[string(key)] = tValue{
		value:   []byte{},
		deleted: true,
	}
	return nil
}
func (f *FastDB) Hash() common.Hash {
	if f.cachedHash.Big().Uint64() != 0 {
		return f.cachedHash
	}
	keyList := make([]string, 0, len(f.cache))
	for k, _ := range f.cache {
		keyList = append(keyList, k)
	}

	if len(f.cache) == 0 {
		return common.Hash{}
	}
	seed := make([]byte, 0)
	for _, k := range keyList {
		seed = append(seed, []byte(k)...)
		seed = append(seed, f.cache[k].value...)
	}
	f.cachedHash = common.BytesToHash(crypto.Keccak256(seed))
	return f.cachedHash
}

func (f *FastDB) Commit(onleaf LeafCallback) (common.Hash, error) {
	batch := f.db.diskdb.NewBatch()
	for k, v := range f.cache {
		if v.deleted {
			batch.Delete([]byte(k))
		} else {
			batch.Put([]byte(k), v.value)
		}
	}
	batch.Write()
	return f.Hash(), nil
}
func (f *FastDB) NodeIterator(startKey []byte) NodeIterator {
	panic("fastdb NodeIterator not implement")
}
func (f *FastDB) Prove(key []byte, fromLevel uint, proofDb ethdb.KeyValueWriter) error {
	panic("fastdb Prove not implement")
}
