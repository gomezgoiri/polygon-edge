package itrie

import (
	"bytes"
	"fmt"

	"github.com/umbracle/minimal/helper/hex"
	"github.com/umbracle/minimal/rlp"
	"github.com/umbracle/minimal/rlpv2"
	"github.com/umbracle/minimal/state"
	"github.com/umbracle/minimal/types"
	"golang.org/x/crypto/sha3"

	iradix "github.com/hashicorp/go-immutable-radix"
)

// Node represents a node reference
type Node interface {
	Hash() ([]byte, bool)
	SetHash(b []byte) []byte
}

// ValueNode is a leaf on the merkle-trie
type ValueNode struct {
	// hash marks if this value node represents a stored node
	hash bool
	buf  []byte
}

// Hash implements the node interface
func (v *ValueNode) Hash() ([]byte, bool) {
	return v.buf, v.hash
}

// SetHash implements the node interface
func (v *ValueNode) SetHash(b []byte) []byte {
	panic("We cannot set hash on value node")
}

type common struct {
	hash []byte
}

// Hash implements the node interface
func (c *common) Hash() ([]byte, bool) {
	return c.hash, len(c.hash) != 0
}

// SetHash implements the node interface
func (c *common) SetHash(b []byte) []byte {
	c.hash = extendByteSlice(c.hash, len(b))
	copy(c.hash, b)
	return c.hash
}

// ShortNode is an extension or short node
type ShortNode struct {
	common
	key   []byte
	child Node
}

// FullNode is a node with several children
type FullNode struct {
	common
	epoch    uint32
	value    Node
	children [16]Node
}

func (f *FullNode) replaceEdge(idx byte, e Node) {
	if idx == 16 {
		f.value = e
	} else {
		f.children[idx] = e
	}
}

func (f *FullNode) setEdge(idx byte, e Node) {
	if idx == 16 {
		f.value = e
	} else {
		f.children[idx] = e
	}
}

func (f *FullNode) getEdge(idx byte) Node {
	if idx == 16 {
		return f.value
	} else {
		return f.children[idx]
	}
}

type Trie struct {
	state   *State
	root    Node
	epoch   uint32
	storage Storage
}

func NewTrie() *Trie {
	return &Trie{}
}

func (t *Trie) Get(k []byte) ([]byte, bool) {
	txn := t.Txn()
	res := txn.Lookup(k)
	return res, res != nil
}

func hashit(k []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(k)
	return h.Sum(nil)
}

var accountArenaPool rlpv2.ArenaPool

func (t *Trie) Commit(x *iradix.Tree) (state.Snapshot, []byte) {
	// Create an insertion batch for all the entries
	batch := t.storage.Batch()

	tt := t.Txn()
	tt.batch = batch

	arena := accountArenaPool.Get()
	defer accountArenaPool.Put(arena)

	x.Root().Walk(func(k []byte, v interface{}) bool {
		a, ok := v.(*state.StateObject)
		if !ok {
			// We also have logs, avoid those
			return false
		}

		if a.Deleted {
			tt.Delete(hashit(k))
			return false
		}

		// compute first the state changes
		if a.Txn != nil {
			localTxn := a.Account.Trie.(*Trie).Txn()
			localTxn.batch = batch

			// Apply all the changes
			a.Txn.Root().Walk(func(k []byte, v interface{}) bool {
				if v == nil {
					localTxn.Delete(k)
				} else {
					vv, _ := rlp.EncodeToBytes(bytes.TrimLeft(v.([]byte), "\x00"))
					localTxn.Insert(k, vv)
				}
				return false
			})

			accountStateRoot, _ := localTxn.Hash()
			accountStateTrie := localTxn.Commit()

			// Add this to the cache
			t.state.AddState(types.BytesToHash(accountStateRoot), accountStateTrie)

			a.Account.Root = types.BytesToHash(accountStateRoot)
		}

		if a.DirtyCode {
			t.state.SetCode(types.BytesToHash(a.Account.CodeHash), a.Code)
		}

		vv := arena.NewArray()
		vv.Set(arena.NewUint(a.Account.Nonce))
		vv.Set(arena.NewBigInt(a.Account.Balance))
		vv.Set(arena.NewBytes(a.Account.Root.Bytes()))
		vv.Set(arena.NewBytes(a.Account.CodeHash))

		data := vv.MarshalTo(nil)

		tt.Insert(hashit(k), data)
		arena.Reset()
		return false
	})

	root, _ := tt.Hash()

	nTrie := tt.Commit()
	nTrie.state = t.state
	nTrie.storage = t.storage

	// Write all the entries to db
	batch.Write()

	t.state.AddState(types.BytesToHash(root), nTrie)
	return nTrie, root
}

func (t *Trie) Txn() *Txn {
	return &Txn{root: t.root, epoch: t.epoch + 1, storage: t.storage}
}

type Putter interface {
	Put(k, v []byte)
}

type Txn struct {
	root    Node
	epoch   uint32
	storage Storage
	batch   Putter
}

func (t *Txn) Commit() *Trie {
	return &Trie{epoch: t.epoch, root: t.root, storage: t.storage}
}

func (t *Txn) Lookup(key []byte) []byte {
	return t.lookup(t.root, keybytesToHex(key))
}

func (t *Txn) lookup(node interface{}, key []byte) []byte {
	switch n := node.(type) {
	case nil:
		return nil

	case *ValueNode:
		if n.hash {
			nc, ok, err := GetNode(n.buf, t.storage)
			if err != nil {
				panic(err)
			}
			if !ok {
				return nil
			}
			node = nc
			return t.lookup(node, key)
		}

		if len(key) == 0 {
			return n.buf
		} else {
			return nil
		}

	case *ShortNode:
		plen := len(n.key)
		if plen > len(key) || !bytes.Equal(key[:plen], n.key) {
			return nil
		} else {
			return t.lookup(n.child, key[plen:])
		}

	case *FullNode:
		if len(key) == 0 {
			return t.lookup(n.value, key)
		} else {
			return t.lookup(n.getEdge(key[0]), key[1:])
		}

	default:
		panic(fmt.Sprintf("unknown node type %v", n))
	}
}

func (t *Txn) writeNode(n *FullNode) *FullNode {
	if t.epoch == n.epoch {
		return n
	}

	nc := &FullNode{
		epoch: t.epoch,
		value: n.value,
	}
	copy(nc.children[:], n.children[:])
	return nc
}

func (t *Txn) Insert(key, value []byte) {
	root := t.insert(t.root, keybytesToHex(key), value)
	if root != nil {
		t.root = root
	}
}

func (t *Txn) insert(node Node, search, value []byte) Node {
	switch n := node.(type) {
	case nil:
		// NOTE, this only happens with the full node
		if len(search) == 0 {
			v := &ValueNode{}
			v.buf = make([]byte, len(value))
			copy(v.buf, value)
			return v
		} else {
			return &ShortNode{
				key:   search,
				child: t.insert(nil, nil, value),
			}
		}

	case *ValueNode:
		if n.hash {
			nc, ok, err := GetNode(n.buf, t.storage)
			if err != nil {
				panic(err)
			}
			if !ok {
				return nil
			}
			node = nc
			return t.insert(node, search, value)
		}

		if len(search) == 0 {
			v := &ValueNode{}
			v.buf = make([]byte, len(value))
			copy(v.buf, value)
			return v
		} else {
			b := t.insert(&FullNode{epoch: t.epoch, value: n}, search, value)
			return b
		}

	case *ShortNode:
		plen := prefixLen(search, n.key)
		if plen == len(n.key) {
			// Keep this node as is and insert to child
			child := t.insert(n.child, search[plen:], value)
			return &ShortNode{key: n.key, child: child}

		} else {
			// Introduce a new branch
			b := FullNode{epoch: t.epoch}
			if len(n.key) > plen+1 {
				b.setEdge(n.key[plen], &ShortNode{key: n.key[plen+1:], child: n.child})
			} else {
				b.setEdge(n.key[plen], n.child)
			}

			child := t.insert(&b, search[plen:], value)

			if plen == 0 {
				return child
			} else {
				return &ShortNode{key: search[:plen], child: child}
			}
		}

	case *FullNode:
		b := t.writeNode(n)

		if len(search) == 0 {
			b.value = t.insert(b.value, nil, value)
			return b
		} else {
			k := search[0]
			child := n.getEdge(k)
			newChild := t.insert(child, search[1:], value)
			if child == nil {
				b.setEdge(k, newChild)
			} else {
				b.replaceEdge(k, newChild)
			}
			return b
		}

	default:
		panic(fmt.Sprintf("unknown node type %v", n))
	}
}

func (t *Txn) Delete(key []byte) {
	root, ok := t.delete(t.root, keybytesToHex(key))
	if ok {
		t.root = root
	}
}

func (t *Txn) delete(node Node, search []byte) (Node, bool) {
	switch n := node.(type) {
	case nil:
		return nil, false

	case *ShortNode:
		n.hash = n.hash[:0]

		plen := prefixLen(search, n.key)
		if plen == len(search) {
			return nil, true
		}
		if plen == 0 {
			return nil, false
		}

		child, ok := t.delete(n.child, search[plen:])
		if !ok {
			return nil, false
		}
		if child == nil {
			return nil, true
		}
		if short, ok := child.(*ShortNode); ok {
			// merge nodes
			return &ShortNode{key: concat(n.key, short.key), child: short.child}, true
		} else {
			// full node
			n.child = child
			return n, true
		}

	case *ValueNode:
		if n.hash {
			nc, ok, err := GetNode(n.buf, t.storage)
			if err != nil {
				panic(err)
			}
			if !ok {
				return nil, false
			}
			node = nc
			return t.delete(node, search)
		}
		if len(search) != 0 {
			return nil, false
		}
		return nil, true

	case *FullNode:
		n.hash = n.hash[:0]

		key := search[0]
		newChild, ok := t.delete(n.getEdge(key), search[1:])
		if !ok {
			return nil, false
		}

		n.setEdge(key, newChild)
		indx := -1
		var notEmpty bool

		for edge, i := range n.children {
			if i != nil {
				if indx != -1 {
					notEmpty = true
					break
				} else {
					indx = edge
				}
			}
		}
		if indx != -1 && n.value != nil {
			// We have one children and value, set notEmpty to true
			notEmpty = true
		}
		if notEmpty {
			// fmt.Println("- node is not empty -")
			// The full node still has some other values
			return n, true
		}
		if indx == -1 {
			// There are no children nodes
			if n.value == nil {
				// Everything is empty, return nil
				return nil, true
			}
			// The value is the only left, return a short node with it
			return &ShortNode{key: []byte{0x10}, child: n.value}, true
		}

		// Only one value left at indx
		nc := n.children[indx]

		if vv, ok := nc.(*ValueNode); ok && vv.hash {
			// If the value is a hash, we have to resolve it first.
			// This needs better testing
			aux, ok, err := GetNode(vv.buf, t.storage)
			if err != nil {
				panic(err)
			}
			if !ok {
				return nil, false
			}
			nc = aux
		}

		obj, ok := nc.(*ShortNode)
		if !ok {
			obj := &ShortNode{}
			obj.key = []byte{byte(indx)}
			obj.child = nc
			return obj, true
		}

		// Its a short node, compress the full node (now a short node) with the short in children.
		obj.key = concat([]byte{byte(indx)}, obj.key)

		// You are using the child as the new node, but if this one has a hash it belongs
		// to his position in a lower part of the trie, reset the value
		obj.hash = obj.hash[:0]
		return obj, true
	}

	// fmt.Println(node)
	panic("it should not happen")
}

func (t *Txn) Show() {
	show(t.root, 0, 0)
}

func prefixLen(k1, k2 []byte) int {
	max := len(k1)
	if l := len(k2); l < max {
		max = l
	}
	var i int
	for i = 0; i < max; i++ {
		if k1[i] != k2[i] {
			break
		}
	}
	return i
}

func concat(a, b []byte) []byte {
	c := make([]byte, len(a)+len(b))
	copy(c, a)
	copy(c[len(a):], b)
	return c
}

func depth(d int) string {
	s := ""
	for i := 0; i < d; i++ {
		s += "\t"
	}
	return s
}

func show(obj interface{}, label int, d int) {
	switch n := obj.(type) {
	case *ShortNode:
		if h, ok := n.Hash(); ok {
			fmt.Printf("%s%d SHash: %s\n", depth(d), label, hex.EncodeToHex(h))
			return
		}
		fmt.Printf("%s%d Short: %s\n", depth(d), label, hex.EncodeToHex(n.key))
		show(n.child, 0, d)
	case *FullNode:
		if h, ok := n.Hash(); ok {
			fmt.Printf("%s%d FHash: %s\n", depth(d), label, hex.EncodeToHex(h))
			return
		}
		fmt.Printf("%s%d Full\n", depth(d), label)
		for indx, i := range n.children {
			if i != nil {
				show(i, indx, d+1)
			}
		}
		if n.value != nil {
			show(n.value, 16, d)
		}
	case *ValueNode:
		if n.hash {
			fmt.Printf("%s%d  Hash: %s\n", depth(d), label, hex.EncodeToHex(n.buf))
		} else {
			fmt.Printf("%s%d  Value: %s\n", depth(d), label, hex.EncodeToHex(n.buf))
		}
	default:
		panic("not expected")
	}
}

func extendByteSlice(b []byte, needLen int) []byte {
	b = b[:cap(b)]
	if n := needLen - cap(b); n > 0 {
		b = append(b, make([]byte, n)...)
	}
	return b[:needLen]
}