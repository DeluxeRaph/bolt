package server

import (
	"github.com/attestantio/go-eth2-client/spec/phase0"
	gethCommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	lru "github.com/hashicorp/golang-lru/v2"
)

type BatchedSignedConstraints = []*SignedConstraints

type SignedConstraints struct {
	Message   ConstraintsMessage  `json:"message"`
	Signature phase0.BLSSignature `json:"signature"`
}

type ConstraintsMessage struct {
	ValidatorIndex uint64        `json:"validator_index"`
	Slot           uint64        `json:"slot"`
	Constraints    []*Constraint `json:"constraints"`
}

type Constraint struct {
	Tx    Transaction `json:"tx"`
	Index *uint64     `json:"index"`
}

func (s *SignedConstraints) String() string {
	return JSONStringify(s)
}

func (m *ConstraintsMessage) String() string {
	return JSONStringify(m)
}

func (c *Constraint) String() string {
	return JSONStringify(c)
}

// ConstraintCache is a cache for constraints.
type ConstraintCache struct {
	// map of slots to all constraints for that slot
	constraints *lru.Cache[uint64, map[gethCommon.Hash]*Constraint]
}

// NewConstraintCache creates a new constraint cache.
// cap is the maximum number of slots to store constraints for.
func NewConstraintCache(cap int) *ConstraintCache {
	constraints, _ := lru.New[uint64, map[gethCommon.Hash]*Constraint](cap)
	return &ConstraintCache{
		constraints: constraints,
	}
}

// AddInclusionConstraint adds an inclusion constraint to the cache at the given slot for the given transaction.
func (c *ConstraintCache) AddInclusionConstraint(slot uint64, tx Transaction, index *uint64) error {
	if _, exists := c.constraints.Get(slot); !exists {
		c.constraints.Add(slot, make(map[gethCommon.Hash]*Constraint))
	}

	// parse transaction to get its hash and store it in the cache
	// for constant time lookup later
	parsedTx := new(types.Transaction)
	err := parsedTx.UnmarshalBinary(tx)
	if err != nil {
		return err
	}

	m, _ := c.constraints.Get(slot)
	m[parsedTx.Hash()] = &Constraint{
		Tx:    tx,
		Index: index,
	}

	return nil
}

// AddInclusionConstraints adds multiple inclusion constraints to the cache at the given slot
func (c *ConstraintCache) AddInclusionConstraints(slot uint64, constraints []*Constraint) error {
	if _, exists := c.constraints.Get(slot); !exists {
		c.constraints.Add(slot, make(map[gethCommon.Hash]*Constraint))
	}

	m, _ := c.constraints.Get(slot)
	for _, constraint := range constraints {
		parsedTx := new(types.Transaction)
		err := parsedTx.UnmarshalBinary(constraint.Tx)
		if err != nil {
			return err
		}
		m[parsedTx.Hash()] = constraint
	}

	return nil
}

// Get gets the constraints at the given slot.
func (c *ConstraintCache) Get(slot uint64) (map[gethCommon.Hash]*Constraint, bool) {
	return c.constraints.Get(slot)
}

// FindTransactionByHash finds the constraint for the given transaction hash and returns it.
func (c *ConstraintCache) FindTransactionByHash(txHash gethCommon.Hash) (*Constraint, bool) {
	for _, hashToConstraint := range c.constraints.Values() {
		if constraint, exists := hashToConstraint[txHash]; exists {
			return constraint, true
		}
	}
	return nil, false
}

type (
	HashToConstraintDecoded = map[gethCommon.Hash]*ConstraintDecoded
	ConstraintDecoded       struct {
		Index *uint64
		Tx    *types.Transaction
	}
)
