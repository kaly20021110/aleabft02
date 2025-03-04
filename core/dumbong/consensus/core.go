package consensus

import (
	"bft/mvba/core"
	"bft/mvba/crypto"
	"bft/mvba/logger"
	"bft/mvba/pool"
	"bft/mvba/store"
	"sync"
)

type Core struct {
	Name       core.NodeID
	Committee  core.Committee
	Parameters core.Parameters
	SigService *crypto.SigService
	Store      *store.Store
	TxPool     *pool.Pool
	Transimtor *core.Transmitor
	Aggreator  *Aggreator
	Elector    *Elector
	Commitor   *Committor

	Height       int64                 //the height of block, height++, then create block, the height of the first block is 1
	CurrentCert  map[core.NodeID]int64 //map from sender to height
	OrderedCert  map[core.NodeID]int64
	BlockHashMap map[core.NodeID]map[int64]crypto.Digest //map from NodeID to height to blockHash

	FinishFlags  map[int64]map[int64]map[core.NodeID]crypto.Digest // finish? map[epoch][round][node] = blockHash
	SPbInstances map[int64]map[int64]map[core.NodeID]*SPB          // map[epoch][node][round]
	DoneFlags    map[int64]map[int64]struct{}
	ReadyFlags   map[int64]map[int64]struct{}
	HaltFlags    map[int64]struct{}
	Epoch        int64

	blocklock sync.Mutex
	smvbaflag bool
}

func NewCore(
	Name core.NodeID,
	Committee core.Committee,
	Parameters core.Parameters,
	SigService *crypto.SigService,
	Store *store.Store,
	TxPool *pool.Pool,
	Transimtor *core.Transmitor,
	callBack chan<- struct{},
) *Core {

	c := &Core{
		Name:         Name,
		Committee:    Committee,
		Parameters:   Parameters,
		SigService:   SigService,
		Store:        Store,
		TxPool:       TxPool,
		Transimtor:   Transimtor,
		Epoch:        0,
		Height:       0,
		BlockHashMap: make(map[core.NodeID]map[int64]crypto.Digest),
		Aggreator:    NewAggreator(Committee),
		Elector:      NewElector(SigService, Committee),
		Commitor:     NewCommittor(callBack),
		FinishFlags:  make(map[int64]map[int64]map[core.NodeID]crypto.Digest),
		SPbInstances: make(map[int64]map[int64]map[core.NodeID]*SPB),
		DoneFlags:    make(map[int64]map[int64]struct{}),
		ReadyFlags:   make(map[int64]map[int64]struct{}),
		HaltFlags:    make(map[int64]struct{}),
		smvbaflag:    false, //true: running; false: finished
	}
	c.initCert()

	return c
}

func (c *Core) initCert() {
	c.OrderedCert = make(map[core.NodeID]int64) //map from sender to height
	c.CurrentCert = make(map[core.NodeID]int64) //map from sender to height
	for id := range c.Committee.Authorities {
		c.CurrentCert[id] = 0
		c.OrderedCert[id] = 0
	}
}

func (c *Core) messgaeFilter(epoch int64) bool {
	return epoch < c.Epoch
}

func (c *Core) storeBlock(block *Block) error {
	key := block.Hash()
	value, err := block.Encode()
	if err != nil {
		return err
	}
	return c.Store.Write(key[:], value)
}

func (c *Core) getBlock(digest crypto.Digest) (*Block, error) {
	value, err := c.Store.Read(digest[:])

	if err == store.ErrNotFoundKey {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	b := &Block{}
	if err := b.Decode(value); err != nil {
		return nil, err
	}
	return b, err
}

func (c *Core) storeSMVBABlock(block *SMVBABlock) error {
	key := block.Hash()
	value, err := block.Encode()
	if err != nil {
		return err
	}
	return c.Store.Write(key[:], value)
}

func (c *Core) getSMVBABlock(digest crypto.Digest) (*SMVBABlock, error) {
	value, err := c.Store.Read(digest[:])

	if err == store.ErrNotFoundKey {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	b := &SMVBABlock{}
	if err := b.Decode(value); err != nil {
		return nil, err
	}
	return b, err
}

func (c *Core) getSpbInstance(epoch, round int64, node core.NodeID) *SPB {
	rItems, ok := c.SPbInstances[epoch]
	if !ok {
		rItems = make(map[int64]map[core.NodeID]*SPB)
		c.SPbInstances[epoch] = rItems
	}
	instances, ok := rItems[round]
	if !ok {
		instances = make(map[core.NodeID]*SPB)
		rItems[round] = instances
	}
	instance, ok := instances[node]
	if !ok {
		instance = NewSPB(c, epoch, round, node)
		instances[node] = instance
	}

	return instance
}

func (c *Core) hasFinish(epoch, round int64, node core.NodeID) (bool, crypto.Digest) {
	if items, ok := c.FinishFlags[epoch]; !ok {
		return false, crypto.Digest{}
	} else {
		if item, ok := items[round]; !ok {
			return false, crypto.Digest{}
		} else {
			d, ok := item[node]
			return ok, d
		}
	}
}

func (c *Core) hasReady(epoch, round int64) bool {
	if items, ok := c.ReadyFlags[epoch]; !ok {
		c.ReadyFlags[epoch] = make(map[int64]struct{})
		return false
	} else {
		_, ok = items[round]
		return ok
	}
}

func (c *Core) hasDone(epoch, round int64) bool {
	if items, ok := c.DoneFlags[epoch]; !ok {
		c.DoneFlags[epoch] = make(map[int64]struct{})
		return false
	} else {
		_, ok = items[round]
		return ok
	}
}

func (c *Core) generatorBlock(height int64, preHash crypto.Digest) *Block {
	block := NewBlock(c.Name, c.TxPool.GetBatch(), height, preHash)
	if block.Batch.ID != -1 {
		logger.Info.Printf("create Block epoch %d node %d batch_id %d \n", block.Height, block.Proposer, block.Batch.ID)
	}
	return block
}

/*********************************** Broadcast Block Start***************************************/
func (c *Core) PbBroadcastBlock(preHash crypto.Digest) error {
	c.Height++
	logger.Debug.Printf("Broadcast Block node %d height %d\n", c.Name, c.Height)
	block := c.generatorBlock(c.Height, preHash)
	bm, _ := NewBlockMessage(c.Name, block, c.Height, c.SigService)
	c.Transimtor.Send(c.Name, core.NONE, bm)
	c.Transimtor.RecvChannel() <- bm
	return nil
}

func (c *Core) handleBlockMessage(bm *BlockMessage) error {
	c.blocklock.Lock()
	defer c.blocklock.Unlock()
	logger.Debug.Printf("Processing BlockMessage proposer %d height %d\n", bm.Author, bm.Height)

	c.CurrentCert[bm.Author] = bm.Height - 1
	if _, ok := c.BlockHashMap[bm.Author]; !ok {
		c.BlockHashMap[bm.Author] = make(map[int64]crypto.Digest)
	}
	c.BlockHashMap[bm.Author][bm.Height] = bm.B.Hash()
	if err := c.storeBlock(bm.B); err != nil {
		logger.Error.Printf("Store Block error: %v\n", err)
		return err
	}
	go c.PbSendVote(bm.Author, bm.Height, bm.B.Hash())
	go c.TrytoStartMVBA()
	return nil
}

func (c *Core) PbSendVote(to core.NodeID, height int64, blockHash crypto.Digest) error {
	vote, _ := NewVoteforBlock(c.Name, blockHash, height, c.SigService)
	c.Transimtor.Send(c.Name, to, vote)
	return nil
}

func (c *Core) handleVoteForBlock(v *VoteforBlock) error {
	c.blocklock.Lock()
	defer c.blocklock.Unlock()
	logger.Debug.Printf("Processing VoteforBlock proposer %d height %d\n", v.Author, v.Height)
	if v.Height < c.Height {
		return nil
	}
	if flag, err := c.Aggreator.AddBlockVote(v); err != nil {
		logger.Warn.Println(err)
		return err
	} else if flag == BV_HIGH_FLAG {
		logger.Debug.Printf("Enough VoteforBlock node %d height %d\n", c.Name, v.Height)
		go c.PbBroadcastBlock(v.BlockHash)
		return nil
	} else {
		return nil
	}
}

/*********************************** Broadcast Block End***************************************/

func (c *Core) TrytoStartMVBA() {
	c.blocklock.Lock()
	defer c.blocklock.Unlock()
	if c.smvbaflag {
		return
	}
	smvbaproposal, newCount := c.generatesmvbaproposal()
	if newCount >= c.Committee.HightThreshold() {
		c.smvbaflag = true
		smvbablock := NewSMVBABlock(c.Name, smvbaproposal, c.Epoch)
		spbproposal, _ := NewSPBProposal(c.Name, smvbablock, c.Epoch, 0, SPB_ONE_PHASE, c.SigService)
		if err := c.Transimtor.Send(c.Name, core.NONE, spbproposal); err != nil {
			panic(err)
		}
		c.Transimtor.RecvChannel() <- spbproposal
	}
}

func (c *Core) generatesmvbaproposal() (map[core.NodeID]*CertForBlockData, int) {
	var newCount = 0
	proposal := make(map[core.NodeID]*CertForBlockData)
	for id, height := range c.CurrentCert {
		if c.CurrentCert[id] > c.OrderedCert[id] {
			newCount++
		}
		var certforblock CertForBlockData
		if height == 0 {
			certforblock = CertForBlockData{0, crypto.Digest{}}
		} else {
			certforblock = CertForBlockData{height, c.BlockHashMap[id][height]}
		}
		proposal[id] = &certforblock
	}
	return proposal, newCount
}

func (c *Core) commitSMVBABlock(block *SMVBABlock) error {
	proposal := block.SMVBAProposal
	for id, cert := range proposal {
		if cert.Height > c.OrderedCert[id] {
			for i := c.OrderedCert[id] + 1; i <= cert.Height; i++ {
				c.blocklock.Lock()
				blockhash, ok := c.BlockHashMap[id][i]
				c.blocklock.Unlock()
				if !ok {
					logger.Info.Printf("No block when commit height %d author %d\n", i, id)
					continue
				}
				if block, err := c.getBlock(blockhash); err != nil {
					return err
				} else {
					c.Commitor.commitCh <- block
				}
			}
			c.OrderedCert[id] = cert.Height
		}
	}
	return nil
}

/*********************************** Protocol Start***************************************/
func (c *Core) handleSpbProposal(p *SPBProposal) error {
	logger.Debug.Printf("Processing SPBProposal proposer %d epoch %d round %d phase %d\n", p.Author, p.Epoch, p.Round, p.Phase)

	//ensure all block is received
	if p.Phase == SPB_ONE_PHASE {
		if _, ok := c.HaltFlags[p.Epoch]; ok {
			if leader := c.Elector.Leader(p.Epoch, p.Round); leader == p.Author {
				//c.Commitor.Commit(p.B)
				c.commitSMVBABlock(p.B)
			}
		}
	}

	//discard message
	if c.messgaeFilter(p.Epoch) {
		return nil
	}

	//Store Block at first time
	if p.Phase == SPB_ONE_PHASE {
		if err := c.storeSMVBABlock(p.B); err != nil {
			logger.Error.Printf("Store Block error: %v\n", err)
			return err
		}
	}

	spb := c.getSpbInstance(p.Epoch, p.Round, p.Author)
	go spb.processProposal(p)

	return nil
}

func (c *Core) handleSpbVote(v *SPBVote) error {
	logger.Debug.Printf("Processing SPBVote proposer %d epoch %d round %d phase %d\n", v.Proposer, v.Epoch, v.Round, v.Phase)

	//discard message
	if c.messgaeFilter(v.Epoch) {
		return nil
	}

	spb := c.getSpbInstance(v.Epoch, v.Round, v.Proposer)
	go spb.processVote(v)

	return nil
}

func (c *Core) handleFinish(f *Finish) error {
	logger.Debug.Printf("Processing Finish epoch %d round %d\n", f.Epoch, f.Round)

	//discard message
	if c.messgaeFilter(f.Epoch) {
		return nil
	}
	if flag, err := c.Aggreator.AddFinishVote(f); err != nil {
		return err
	} else {
		rF, ok := c.FinishFlags[f.Epoch]
		if !ok {
			rF = make(map[int64]map[core.NodeID]crypto.Digest)
			c.FinishFlags[f.Epoch] = rF
		}
		nF, ok := rF[f.Round]
		if !ok {
			nF = make(map[core.NodeID]crypto.Digest)
			rF[f.Round] = nF
		}
		nF[f.Author] = f.BlockHash
		if flag {
			return c.invokeDoneAndShare(f.Epoch, f.Round)
		}
	}

	return nil
}

func (c *Core) invokeDoneAndShare(epoch, round int64) error {
	logger.Debug.Printf("Processing invoke Done and Share epoch %d,round %d\n", epoch, round)

	if !c.hasDone(epoch, round) {

		done, _ := NewDone(c.Name, epoch, round, c.SigService)
		share, _ := NewElectShare(c.Name, epoch, round, c.SigService)

		c.Transimtor.Send(c.Name, core.NONE, done)
		c.Transimtor.Send(c.Name, core.NONE, share)
		c.Transimtor.RecvChannel() <- done
		c.Transimtor.RecvChannel() <- share

		items, ok := c.DoneFlags[epoch]
		if !ok {
			items = make(map[int64]struct{})
			c.DoneFlags[epoch] = items
		}
		items[round] = struct{}{}
	}

	return nil
}

func (c *Core) handleDone(d *Done) error {
	logger.Debug.Printf("Processing Done epoch %d round %d\n", d.Epoch, d.Round)

	//discard message
	if c.messgaeFilter(d.Epoch) {
		return nil
	}

	if flag, err := c.Aggreator.AddDoneVote(d); err != nil {
		return err
	} else if flag == DONE_LOW_FLAG {
		return c.invokeDoneAndShare(d.Epoch, d.Round)
	} else if flag == DONE_HIGH_FLAG {
		items, ok := c.ReadyFlags[d.Epoch]
		if !ok {
			items = make(map[int64]struct{})
			c.ReadyFlags[d.Epoch] = items
		}
		items[d.Round] = struct{}{}
		return c.processLeader(d.Epoch, d.Round)
	}

	return nil
}

func (c *Core) handleElectShare(share *ElectShare) error {
	logger.Debug.Printf("Processing ElectShare epoch %d round %d\n", share.Epoch, share.Round)

	//discard message
	if c.messgaeFilter(share.Epoch) {
		return nil
	}

	if leader, err := c.Elector.AddShareVote(share); err != nil {
		return err
	} else if leader != core.NONE {
		c.processLeader(share.Epoch, share.Round)
	}

	return nil
}

func (c *Core) processLeader(epoch, round int64) error {
	logger.Debug.Printf("Processing Leader epoch %d round %d Leader %d\n", epoch, round, c.Elector.Leader(epoch, round))
	if c.hasReady(epoch, round) {
		if leader := c.Elector.Leader(epoch, round); leader != core.NONE {
			if ok, d := c.hasFinish(epoch, round, leader); ok {
				//send halt
				halt, _ := NewHalt(c.Name, leader, d, epoch, round, c.SigService)
				c.Transimtor.Send(c.Name, core.NONE, halt)
				c.Transimtor.RecvChannel() <- halt
			} else {
				//send preVote
				var preVote *Prevote
				if spb := c.getSpbInstance(epoch, round, leader); spb.IsLock() {
					if blockHash, ok := spb.GetBlockHash().(crypto.Digest); !ok {
						panic("block hash is nil")
					} else {
						preVote, _ = NewPrevote(c.Name, leader, epoch, round, VOTE_FLAG_YES, blockHash, c.SigService)
					}
				} else {
					preVote, _ = NewPrevote(c.Name, leader, epoch, round, VOTE_FLAG_NO, crypto.Digest{}, c.SigService)
				}
				c.Transimtor.Send(c.Name, core.NONE, preVote)
				c.Transimtor.RecvChannel() <- preVote
			}
		}
	}

	return nil
}

func (c *Core) handlePrevote(pv *Prevote) error {
	logger.Debug.Printf("Processing Prevote epoch %d round %d\n", pv.Epoch, pv.Round)

	//discard message
	if c.messgaeFilter(pv.Epoch) {
		return nil
	}

	if flag, err := c.Aggreator.AddPreVote(pv); err != nil {
		return err
	} else if flag == ACTION_NO {
		vote, _ := NewFinVote(c.Name, pv.Leader, pv.Epoch, pv.Round, VOTE_FLAG_NO, pv.BlockHash, c.SigService)
		c.Transimtor.Send(c.Name, core.NONE, vote)
		c.Transimtor.RecvChannel() <- vote
	} else if flag == ACTION_YES {
		vote, _ := NewFinVote(c.Name, pv.Leader, pv.Epoch, pv.Round, VOTE_FLAG_YES, pv.BlockHash, c.SigService)
		c.Transimtor.Send(c.Name, core.NONE, vote)
		c.Transimtor.RecvChannel() <- vote
	}

	return nil
}

func (c *Core) handleFinvote(fv *FinVote) error {
	logger.Debug.Printf("Processing FinVote epoch %d round %d\n", fv.Epoch, fv.Round)

	//discard message
	if c.messgaeFilter(fv.Epoch) {
		return nil
	}

	if flag, err := c.Aggreator.AddFinVote(fv); err != nil {
		return err
	} else if flag == ACTION_YES {
		return c.advanceNextRound(fv.Epoch, fv.Round, flag, fv.BlockHash)
	} else if flag == ACTION_NO {
		return c.advanceNextRound(fv.Epoch, fv.Round, flag, crypto.Digest{})
	} else if flag == ACTION_COMMIT {
		halt, _ := NewHalt(c.Name, fv.Leader, fv.BlockHash, fv.Epoch, fv.Round, c.SigService)
		c.Transimtor.Send(c.Name, core.NONE, halt)
		c.Transimtor.RecvChannel() <- halt
	}

	return nil
}

func (c *Core) advanceNextRound(epoch, round int64, flag int8, blockHash crypto.Digest) error {
	logger.Debug.Printf("Processing next round [epoch %d round %d]\n", epoch, round)

	//discard message
	if c.messgaeFilter(epoch) {
		return nil
	}

	if flag == ACTION_NO { //next round block self
		if inte := c.getSpbInstance(epoch, round, c.Name).GetBlockHash(); inte != nil {
			blockHash = inte.(crypto.Digest)
		}
	}

	var proposal *SPBProposal
	if block, err := c.getSMVBABlock(blockHash); err != nil {
		return err
	} else if block != nil {
		proposal, _ = NewSPBProposal(c.Name, block, epoch, round+1, SPB_ONE_PHASE, c.SigService)
	} else {
		c.blocklock.Lock()
		smvbapropasal, _ := c.generatesmvbaproposal()
		c.blocklock.Unlock()
		smvbablock := NewSMVBABlock(c.Name, smvbapropasal, epoch)
		proposal, _ = NewSPBProposal(c.Name, smvbablock, epoch, round+1, SPB_ONE_PHASE, c.SigService)
	}
	c.Transimtor.Send(c.Name, core.NONE, proposal)
	c.Transimtor.RecvChannel() <- proposal

	return nil
}

func (c *Core) handleHalt(h *Halt) error {
	logger.Debug.Printf("Processing Halt epoch %d\n", h.Epoch)

	//discard message
	if c.messgaeFilter(h.Epoch) {
		return nil
	}

	//Check leader

	if _, ok := c.HaltFlags[h.Epoch]; !ok {
		c.Elector.SetLeader(h.Epoch, h.Round, h.Leader)
		if err := c.handleOutput(h.Epoch, h.BlockHash); err != nil {
			return err
		}
		c.HaltFlags[h.Epoch] = struct{}{}
		c.advanceNextEpoch(h.Epoch + 1)
	}

	return nil
}

func (c *Core) handleOutput(epoch int64, blockHash crypto.Digest) error {
	logger.Debug.Printf("Processing Ouput epoch %d \n", epoch)
	if b, err := c.getSMVBABlock(blockHash); err != nil {
		return err
	} else if b != nil {
		//c.Commitor.Commit(b)
		c.commitSMVBABlock(b)
		// if b.Proposer != c.Name {
		// 	temp := c.getSpbInstance(epoch, 0, c.Name).GetBlockHash()
		// 	if temp != nil {
		// 		if block, err := c.getBlock(temp.(crypto.Digest)); err == nil && block != nil {
		// 			c.TxPool.PutBatch(block.Batch)
		// 		}
		// 	}
		// }
	} else {
		logger.Debug.Printf("Processing retriever epoch %d \n", epoch)
	}

	return nil
}

/*********************************** Protocol End***************************************/
func (c *Core) advanceNextEpoch(epoch int64) {
	c.blocklock.Lock()
	defer c.blocklock.Unlock()
	if epoch <= c.Epoch {
		return
	}
	logger.Debug.Println("advance next epoch", epoch)
	//Clear Something

	c.Epoch = epoch
	c.smvbaflag = false
	// block := c.generatorBlock(epoch)
	// proposal, _ := NewSPBProposal(c.Name, block, epoch, 0, SPB_ONE_PHASE, c.SigService)
	// c.Transimtor.Send(c.Name, core.NONE, proposal)
	// c.Transimtor.RecvChannel() <- proposal
	go c.TrytoStartMVBA()
}

func (c *Core) Run() {
	if c.Name < core.NodeID(c.Parameters.Faults) {
		logger.Debug.Printf("Node %d is faulty\n", c.Name)
		return
	}
	//first proposal
	// block := c.generatorBlock(c.Epoch)
	// proposal, _ := NewSPBProposal(c.Name, block, c.Epoch, 0, SPB_ONE_PHASE, c.SigService)
	// if err := c.Transimtor.Send(c.Name, core.NONE, proposal); err != nil {
	// 	panic(err)
	// }
	// c.Transimtor.RecvChannel() <- proposal
	go c.PbBroadcastBlock(crypto.Digest{})

	recvChannal := c.Transimtor.RecvChannel()
	for {
		var err error
		select {
		case msg := <-recvChannal:
			{
				if validator, ok := msg.(Validator); ok {
					if !validator.Verify(c.Committee) {
						err = core.ErrSignature(msg.MsgType())
						break
					}
				}

				switch msg.MsgType() {

				case SPBProposalType:
					err = c.handleSpbProposal(msg.(*SPBProposal))
				case SPBVoteType:
					err = c.handleSpbVote(msg.(*SPBVote))
				case FinishType:
					err = c.handleFinish(msg.(*Finish))
				case DoneType:
					err = c.handleDone(msg.(*Done))
				case ElectShareType:
					err = c.handleElectShare(msg.(*ElectShare))
				case PrevoteType:
					err = c.handlePrevote(msg.(*Prevote))
				case FinVoteType:
					err = c.handleFinvote(msg.(*FinVote))
				case HaltType:
					err = c.handleHalt(msg.(*Halt))
				case BlockMessageType:
					go c.handleBlockMessage(msg.(*BlockMessage))
				case VoteforBlockType:
					go c.handleVoteForBlock(msg.(*VoteforBlock))

				}
			}
		default:
		}
		if err != nil {
			logger.Warn.Println(err)
		}
	}
}
