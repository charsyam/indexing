// Copyright (c) 2014 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package client

import (
	"errors"
	"fmt"
	"github.com/couchbase/gometa/common"
	"github.com/couchbase/gometa/message"
	"github.com/couchbase/gometa/protocol"
	c "github.com/couchbase/indexing/secondary/common"
	"net"
	"strconv"
	"strings"
	"sync"
)

///////////////////////////////////////////////////////
// Type Definition
///////////////////////////////////////////////////////

type MetadataProvider struct {
	providerId string
	watchers   map[string]*watcher
	repo       *metadataRepo
	mutex      sync.Mutex
}

type metadataRepo struct {
	definitions map[c.IndexDefnId]*c.IndexDefn
	instances   map[c.IndexDefnId]*IndexInstDistribution
	indices     map[c.IndexDefnId]*IndexMetadata
	mutex       sync.Mutex
}

type watcher struct {
	provider   *MetadataProvider
	leaderAddr string
	factory    protocol.MsgFactory
	pendings   map[common.Txnid]protocol.LogEntryMsg
	killch     chan bool
	mutex      sync.Mutex
	indices    map[c.IndexDefnId]interface{}

	incomingReqs chan *protocol.RequestHandle
	pendingReqs  map[uint64]*protocol.RequestHandle // key : request id
	loggedReqs   map[common.Txnid]*protocol.RequestHandle
}

type IndexMetadata struct {
	Definition *c.IndexDefn
	Instances  []*InstanceDefn
}

type InstanceDefn struct {
	InstId c.IndexInstId
	State  c.IndexState
	Error  string
	Endpts []c.Endpoint
}

///////////////////////////////////////////////////////
// Public function : MetadataProvider
///////////////////////////////////////////////////////

func NewMetadataProvider(providerId string) (s *MetadataProvider, err error) {

	s = new(MetadataProvider)
	s.watchers = make(map[string]*watcher)
	s.repo = newMetadataRepo()

	s.providerId, err = s.getWatcherAddr(providerId)
	if err != nil {
		return nil, err
	}
	c.Debugf("MetadataProvider.NewMetadataProvider(): MetadataProvider follower ID %s", s.providerId)

	return s, nil
}

func (o *MetadataProvider) WatchMetadata(indexAdminPort string) {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	_, ok := o.watchers[indexAdminPort]
	if ok {
		return
	}

	o.watchers[indexAdminPort] = o.startWatcher(indexAdminPort)
}

func (o *MetadataProvider) UnwatchMetadata(indexAdminPort string) {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	watcher, ok := o.watchers[indexAdminPort]
	if !ok {
		return
	}

	delete(o.watchers, indexAdminPort)
	if watcher != nil {
		watcher.cleanupIndices(o.repo)
		watcher.close()
	}
}

func (o *MetadataProvider) CreateIndexWithPlan(
	name, bucket, using, exprType, partnExpr, whereExpr string,
	secExprs []string, isPrimary bool, plan map[string]interface{}) (c.IndexDefnId, error) {

	if o.FindIndexByName(name, bucket) != nil {
		return c.IndexDefnId(0), errors.New(fmt.Sprintf("Index %s already exist.", name))
	}

	ns, ok := plan["nodes"].([]interface{})
	if !ok || len(ns) != 1 {
		return c.IndexDefnId(0), errors.New("Create Index is allowed for one and only one node")
	}
	nodes := []string{ns[0].(string)}

	deferred, ok := plan["defer_build"].(bool)
	if !ok {
		deferred = false
	}

	watcher := o.findMatchingWatcher(nodes[0])
	if watcher == nil {
		return c.IndexDefnId(0),
			errors.New(fmt.Sprintf("Fails to create index.  Node %s does not exist or is not running", nodes[0]))
	}

	defnID, err := c.NewIndexDefnId()
	if err != nil {
		return c.IndexDefnId(0), errors.New(fmt.Sprintf("Fails to create index. Fail to create uuid for index definition."))
	}

	idxDefn := &c.IndexDefn{
		DefnId:          defnID,
		Name:            name,
		Using:           c.IndexType(using),
		Bucket:          bucket,
		IsPrimary:       isPrimary,
		SecExprs:        secExprs,
		ExprType:        c.ExprType(exprType),
		PartitionScheme: c.SINGLE,
		PartitionKey:    partnExpr,
		WhereExpr:       whereExpr,
		Deferred:        deferred,
		Nodes:           nodes}

	content, err := c.MarshallIndexDefn(idxDefn)
	if err != nil {
		return 0, err
	}

	key := fmt.Sprintf("%d", defnID)
	err = watcher.makeRequest(OPCODE_CREATE_INDEX, key, content)

	return defnID, err
}

func (o *MetadataProvider) CreateIndex(
	name, bucket, using, exprType, partnExpr, whereExpr, indexAdminPort string,
	secExprs []string, isPrimary bool) (c.IndexDefnId, error) {

	if o.FindIndexByName(name, bucket) != nil {
		return c.IndexDefnId(0), errors.New(fmt.Sprintf("Index %s already exist.", name))
	}

	defnID, err := c.NewIndexDefnId()
	if err != nil {
		return c.IndexDefnId(0), errors.New(fmt.Sprintf("Fails to create index. Fail to create uuid for index definition."))
	}

	idxDefn := &c.IndexDefn{
		DefnId:          defnID,
		Name:            name,
		Using:           c.IndexType(using),
		Bucket:          bucket,
		IsPrimary:       isPrimary,
		SecExprs:        secExprs,
		ExprType:        c.ExprType(exprType),
		WhereExpr:       whereExpr,
		PartitionScheme: c.SINGLE,
		PartitionKey:    partnExpr}

	watcher, err := o.findWatcher(indexAdminPort)
	if err != nil {
		return 0, err
	}

	content, err := c.MarshallIndexDefn(idxDefn)
	if err != nil {
		return 0, err
	}

	key := fmt.Sprintf("%d", defnID)
	err = watcher.makeRequest(OPCODE_CREATE_INDEX, key, content)

	return defnID, err
}

func (o *MetadataProvider) DropIndex(defnID c.IndexDefnId, indexAdminPort string) error {

	if o.FindIndex(defnID) == nil {
		return errors.New("Index does not exist.")
	}

	watcher, err := o.findWatcher(indexAdminPort)
	if err != nil {
		return err
	}

	key := fmt.Sprintf("%d", defnID)
	return watcher.makeRequest(OPCODE_DROP_INDEX, key, []byte(""))
}

func (o *MetadataProvider) BuildIndexes(adminport string, defnIDs []c.IndexDefnId) error {

	for _, id := range defnIDs {
		meta := o.FindIndex(id)
		if meta == nil {
			return errors.New(fmt.Sprintf("Index %s not found", meta.Definition.Name))
		}
		if meta.Instances != nil && meta.Instances[0].State != c.INDEX_STATE_READY {
			return errors.New(fmt.Sprintf("Index %s is not in READY state.", meta.Definition.Name))
		}
	}

	watcher, err := o.findWatcher(adminport)
	if err != nil {
		return err
	}

	list := BuildIndexIdList(defnIDs)
	content, err := MarshallIndexIdList(list)
	if err != nil {
		return err
	}

	return watcher.makeRequest(OPCODE_BUILD_INDEX, "Index Build", content)
}

func (o *MetadataProvider) ListIndex() []*IndexMetadata {
	o.repo.mutex.Lock()
	defer o.repo.mutex.Unlock()

	result := make([]*IndexMetadata, 0, len(o.repo.indices))
	for _, meta := range o.repo.indices {
		if o.isValidIndex(meta) {
			result = append(result, meta)
		}
	}

	return result
}

func (o *MetadataProvider) FindIndex(id c.IndexDefnId) *IndexMetadata {
	o.repo.mutex.Lock()
	defer o.repo.mutex.Unlock()

	if meta, ok := o.repo.indices[id]; ok {
		if o.isValidIndex(meta) {
			return meta
		}
	}

	return nil
}

func (o *MetadataProvider) FindIndexByName(name string, bucket string) *IndexMetadata {
	o.repo.mutex.Lock()
	defer o.repo.mutex.Unlock()

	for _, meta := range o.repo.indices {
		if o.isValidIndex(meta) {
			if meta.Definition.Name == name && meta.Definition.Bucket == bucket {
				return meta
			}
		}
	}

	return nil
}

func (o *MetadataProvider) Close() {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	for _, watcher := range o.watchers {
		watcher.close()
	}
}

///////////////////////////////////////////////////////
// private function : MetadataProvider
///////////////////////////////////////////////////////

func (o *MetadataProvider) startWatcher(addr string) *watcher {

	s := newWatcher(o, addr)
	readych := make(chan bool)

	// TODO: call Close() to cleanup the state upon retry by the MetadataProvider server
	go protocol.RunWatcherServerWithRequest(
		s.leaderAddr,
		s,
		s,
		s.factory,
		s.killch,
		readych)

	// TODO: timeout
	<-readych

	return s
}

func (o *MetadataProvider) findWatcher(indexAdminPort string) (*watcher, error) {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	watcher, ok := o.watchers[indexAdminPort]
	if !ok {
		return nil, errors.New(fmt.Sprintf("MetadataProvider.findWatcher() : Cannot find watcher for index admin %s", indexAdminPort))
	}

	return watcher, nil
}

func (o *MetadataProvider) getWatcherAddr(MetadataProviderId string) (string, error) {

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	if len(addrs) == 0 {
		return "", errors.New("MetadataProvider.getWatcherAddr() : No network address is available")
	}

	for _, addr := range addrs {
		switch s := addr.(type) {
		case *net.IPAddr:
			if s.IP.IsGlobalUnicast() {
				return fmt.Sprintf("%s:indexer:MetadataProvider:%s", addr.String(), MetadataProviderId), nil
			}
		case *net.IPNet:
			if s.IP.IsGlobalUnicast() {
				return fmt.Sprintf("%s:indexer:MetadataProvider:%s", addr.String(), MetadataProviderId), nil
			}
		}
	}

	return "", errors.New("MetadataProvider.getWatcherAddr() : Fail to find an IP address")
}

func (o *MetadataProvider) findMatchingWatcher(deployNodeName string) *watcher {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	for _, watcher := range o.watchers {
		if strings.Index(watcher.leaderAddr, deployNodeName) == 0 {
			return watcher
		}
	}

	return nil
}

func (o *MetadataProvider) isValidIndex(meta *IndexMetadata) bool {

	if meta.Definition == nil {
		return false
	}

	if len(meta.Instances) == 0 {
		return false
	}

	if meta.Instances[0].State == c.INDEX_STATE_CREATED ||
		meta.Instances[0].State == c.INDEX_STATE_DELETED {
		return false
	}

	return true
}

///////////////////////////////////////////////////////
// private function : metadataRepo
///////////////////////////////////////////////////////

func newMetadataRepo() *metadataRepo {

	return &metadataRepo{
		definitions: make(map[c.IndexDefnId]*c.IndexDefn),
		instances:   make(map[c.IndexDefnId]*IndexInstDistribution),
		indices:     make(map[c.IndexDefnId]*IndexMetadata)}
}

func (r *metadataRepo) addDefn(defn *c.IndexDefn) {

	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.definitions[defn.DefnId] = defn
	r.indices[defn.DefnId] = r.makeIndexMetadata(defn)

	inst, ok := r.instances[defn.DefnId]
	if ok {
		r.updateIndexMetadata(defn.DefnId, inst)
	}
}

func (r *metadataRepo) removeDefn(defnId c.IndexDefnId) {

	r.mutex.Lock()
	defer r.mutex.Unlock()

	delete(r.definitions, defnId)
	delete(r.instances, defnId)
	delete(r.indices, defnId)
}

func (r *metadataRepo) updateTopology(topology *IndexTopology) {

	r.mutex.Lock()
	defer r.mutex.Unlock()

	for _, defnRef := range topology.Definitions {
		defnId := c.IndexDefnId(defnRef.DefnId)
		for _, instRef := range defnRef.Instances {
			r.instances[defnId] = &instRef
			r.updateIndexMetadata(defnId, &instRef)
		}
	}
}

func (r *metadataRepo) unmarshallAndAddDefn(content []byte) error {

	defn, err := c.UnmarshallIndexDefn(content)
	if err != nil {
		return err
	}
	r.addDefn(defn)
	return nil
}

func (r *metadataRepo) unmarshallAndAddInst(content []byte) error {

	topology, err := unmarshallIndexTopology(content)
	if err != nil {
		return err
	}
	r.updateTopology(topology)
	return nil
}

func (r *metadataRepo) makeIndexMetadata(defn *c.IndexDefn) *IndexMetadata {

	return &IndexMetadata{Definition: defn,
		Instances: nil}
}

func (r *metadataRepo) updateIndexMetadata(defnId c.IndexDefnId, inst *IndexInstDistribution) {

	meta, ok := r.indices[defnId]
	if ok {
		idxInst := new(InstanceDefn)
		idxInst.InstId = c.IndexInstId(inst.InstId)
		idxInst.State = c.IndexState(inst.State)
		idxInst.Error = inst.Error

		for _, partition := range inst.Partitions {
			for _, slice := range partition.SinglePartition.Slices {
				idxInst.Endpts = append(idxInst.Endpts, c.Endpoint(slice.Host))
			}
		}
		meta.Instances = []*InstanceDefn{idxInst}
	}
}

///////////////////////////////////////////////////////
// private function : Watcher
///////////////////////////////////////////////////////

func newWatcher(o *MetadataProvider, addr string) *watcher {
	s := new(watcher)

	s.provider = o
	s.leaderAddr = addr
	s.killch = make(chan bool, 1) // make it buffered to unblock sender
	s.factory = message.NewConcreteMsgFactory()
	s.pendings = make(map[common.Txnid]protocol.LogEntryMsg)
	s.incomingReqs = make(chan *protocol.RequestHandle)
	s.pendingReqs = make(map[uint64]*protocol.RequestHandle)
	s.loggedReqs = make(map[common.Txnid]*protocol.RequestHandle)
	s.indices = make(map[c.IndexDefnId]interface{})

	return s
}

func (w *watcher) addDefn(defnId c.IndexDefnId) {

	w.mutex.Lock()
	defer w.mutex.Unlock()

	w.indices[defnId] = nil
}

func (w *watcher) removeDefn(defnId c.IndexDefnId) {

	w.mutex.Lock()
	defer w.mutex.Unlock()

	delete(w.indices, defnId)
}

func (w *watcher) addDefnWithNoLock(defnId c.IndexDefnId) {

	w.indices[defnId] = nil
}

func (w *watcher) removeDefnWithNoLock(defnId c.IndexDefnId) {

	delete(w.indices, defnId)
}

func (w *watcher) cleanupIndices(repo *metadataRepo) {

	w.mutex.Lock()
	defer w.mutex.Unlock()

	for defnId, _ := range w.indices {
		repo.removeDefn(defnId)
	}
}

func (w *watcher) close() {

	if len(w.killch) == 0 {
		w.killch <- true
	}
}

func (w *watcher) makeRequest(opCode common.OpCode, key string, content []byte) error {

	uuid, err := c.NewUUID()
	if err != nil {
		return err
	}
	id := uuid.Uint64()

	request := w.factory.CreateRequest(id, uint32(opCode), key, content)

	handle := &protocol.RequestHandle{Request: request, Err: nil}
	handle.CondVar = sync.NewCond(&handle.Mutex)

	handle.CondVar.L.Lock()
	defer handle.CondVar.L.Unlock()

	w.incomingReqs <- handle

	handle.CondVar.Wait()

	return handle.Err
}

///////////////////////////////////////////////////////
// private function
///////////////////////////////////////////////////////

func isIndexDefnKey(key string) bool {
	return strings.Contains(key, "IndexDefinitionId/")
}

func isIndexTopologyKey(key string) bool {
	return strings.Contains(key, "IndexTopology/")
}

///////////////////////////////////////////////////////
// Interface : RequestMgr
///////////////////////////////////////////////////////

func (w *watcher) AddPendingRequest(handle *protocol.RequestHandle) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	// remember the request
	w.pendingReqs[handle.Request.GetReqId()] = handle
}

func (w *watcher) GetRequestChannel() <-chan *protocol.RequestHandle {

	return (<-chan *protocol.RequestHandle)(w.incomingReqs)
}

///////////////////////////////////////////////////////
// Interface : QuorumVerifier
///////////////////////////////////////////////////////

func (w *watcher) HasQuorum(count int) bool {
	return count == 1
}

///////////////////////////////////////////////////////
// Interface : ServerAction
///////////////////////////////////////////////////////

///////////////////////////////////////////////////////
// Server Action for Environment
///////////////////////////////////////////////////////

func (w *watcher) GetEnsembleSize() uint64 {
	return 1
}

func (w *watcher) GetQuorumVerifier() protocol.QuorumVerifier {
	return w
}

///////////////////////////////////////////////////////
// Server Action for Broadcast stage (normal execution)
///////////////////////////////////////////////////////

func (w *watcher) Commit(txid common.Txnid) error {

	w.mutex.Lock()
	defer w.mutex.Unlock()

	msg, ok := w.pendings[txid]
	if !ok {
		c.Warnf("Watcher.commit(): unknown txnid %d.  Txn not processed at commit", txid)
		return nil
	}

	delete(w.pendings, txid)
	err := w.processChange(msg.GetOpCode(), msg.GetKey(), msg.GetContent())

	handle, ok := w.loggedReqs[txid]
	if ok {
		delete(w.loggedReqs, txid)

		handle.CondVar.L.Lock()
		defer handle.CondVar.L.Unlock()

		handle.CondVar.Signal()
	}

	return err
}

func (w *watcher) LogProposal(p protocol.ProposalMsg) error {

	w.mutex.Lock()
	defer w.mutex.Unlock()

	msg := w.factory.CreateLogEntry(p.GetTxnid(), p.GetOpCode(), p.GetKey(), p.GetContent())
	w.pendings[common.Txnid(p.GetTxnid())] = msg

	handle, ok := w.pendingReqs[p.GetReqId()]
	if ok {
		delete(w.pendingReqs, p.GetReqId())
		w.loggedReqs[common.Txnid(p.GetTxnid())] = handle
	}

	return nil
}

func (w *watcher) Abort(fid string, reqId uint64, err string) error {
	w.respond(reqId, err)
	return nil
}

func (w *watcher) Respond(fid string, reqId uint64, err string) error {
	w.respond(reqId, err)
	return nil
}

func (w *watcher) respond(reqId uint64, err string) {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	handle, ok := w.pendingReqs[reqId]
	if ok {
		delete(w.pendingReqs, reqId)

		handle.CondVar.L.Lock()
		defer handle.CondVar.L.Unlock()

		if len(err) != 0 {
			handle.Err = errors.New(err)
		}

		handle.CondVar.Signal()
	}
}

func (w *watcher) GetFollowerId() string {
	return w.provider.providerId
}

func (w *watcher) GetNextTxnId() common.Txnid {
	panic("Calling watcher.GetCommitedEntries() : not supported")
}

///////////////////////////////////////////////////////
// Server Action for retrieving repository state
///////////////////////////////////////////////////////

func (w *watcher) GetLastLoggedTxid() (common.Txnid, error) {
	return common.Txnid(0), nil
}

func (w *watcher) GetLastCommittedTxid() (common.Txnid, error) {
	return common.Txnid(0), nil
}

func (w *watcher) GetStatus() protocol.PeerStatus {
	return protocol.WATCHING
}

func (w *watcher) GetCurrentEpoch() (uint32, error) {
	return 0, nil
}

func (w *watcher) GetAcceptedEpoch() (uint32, error) {
	return 0, nil
}

///////////////////////////////////////////////////////
// Server Action for updating repository state
///////////////////////////////////////////////////////

func (w *watcher) NotifyNewAcceptedEpoch(epoch uint32) error {
	// no-op
	return nil
}

func (w *watcher) NotifyNewCurrentEpoch(epoch uint32) error {
	// no-op
	return nil
}

///////////////////////////////////////////////////////
// Function for discovery phase
///////////////////////////////////////////////////////

func (w *watcher) GetCommitedEntries(txid1, txid2 common.Txnid) (<-chan protocol.LogEntryMsg, <-chan error, chan<- bool, error) {
	panic("Calling watcher.GetCommitedEntries() : not supported")
}

func (w *watcher) LogAndCommit(txid common.Txnid, op uint32, key string, content []byte, toCommit bool) error {

	if err := w.processChange(op, key, content); err != nil {
		c.Errorf("watcher.LogAndCommit(): receive error when processing log entry from server.  Error = %v", err)
	}

	return nil
}

func (w *watcher) processChange(op uint32, key string, content []byte) error {

	c.Debugf("watcher.processChange(): key = %v", key)
	defer c.Debugf("watcher.processChange(): done -> key = %v", key)

	opCode := common.OpCode(op)

	switch opCode {
	case common.OPCODE_ADD, common.OPCODE_SET:
		if isIndexDefnKey(key) {
			if len(content) == 0 {
				c.Debugf("watcher.processChange(): content of key = %v is empty.", key)
			}

			id, err := extractDefnIdFromKey(key)
			if err != nil {
				return err
			}
			w.addDefnWithNoLock(c.IndexDefnId(id))
			return w.provider.repo.unmarshallAndAddDefn(content)

		} else if isIndexTopologyKey(key) {
			if len(content) == 0 {
				c.Debugf("watcher.processChange(): content of key = %v is empty.", key)
			}
			return w.provider.repo.unmarshallAndAddInst(content)
		}
	case common.OPCODE_DELETE:
		if isIndexDefnKey(key) {

			id, err := extractDefnIdFromKey(key)
			if err != nil {
				return err
			}
			w.removeDefnWithNoLock(c.IndexDefnId(id))
			w.provider.repo.removeDefn(c.IndexDefnId(id))
		}
	}

	return nil
}

func extractDefnIdFromKey(key string) (c.IndexDefnId, error) {
	i := strings.Index(key, "/")
	if i != -1 && i < len(key)-1 {
		id, err := strconv.ParseUint(key[i+1:], 10, 64)
		return c.IndexDefnId(id), err
	}

	return c.IndexDefnId(0), errors.New("watcher.processChange() : cannot parse index definition id")
}
