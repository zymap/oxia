package server

import (
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	pb "google.golang.org/protobuf/proto"
	"io"
	"math"
	"oxia/proto"
	"oxia/server/kv"
	"sync"
)

const MaxEpoch = math.MaxUint64

var ErrorInvalidEpoch = errors.New("oxia: invalid epoch")
var ErrorInvalidStatus = errors.New("oxia: invalid status")

// FollowerController handles all the operations of a given shard's follower
type FollowerController interface {
	io.Closer

	// Fence
	//
	// Node handles a fence request
	//
	// A node receives a fencing request, fences itself and responds
	// with its head index.
	//
	// When a node is fenced it cannot:
	// - accept any writes from a client.
	// - accept add entry requests from a leader.
	// - send any entries to followers if it was a leader.
	//
	// Any existing follow cursors are destroyed as is any state
	//regarding reconfigurations.
	Fence(req *proto.FenceRequest) (*proto.FenceResponse, error)

	// Truncate
	//
	// A node that receives a truncate request knows that it
	// has been selected as a follower. It truncates its log
	// to the indicates entry id, updates its epoch and changes
	// to a Follower.
	Truncate(req *proto.TruncateRequest) (*proto.TruncateResponse, error)

	AddEntries(stream proto.OxiaLogReplication_AddEntriesServer) error

	Epoch() uint64
	Status() Status
}

type followerController struct {
	sync.Mutex

	shardId     uint32
	epoch       uint64
	commitIndex EntryId
	headIndex   EntryId
	status      Status
	wal         Wal
	db          kv.DB
	closing     bool
	log         zerolog.Logger
}

func NewFollowerController(shardId uint32, wal Wal, kvFactory kv.KVFactory) (FollowerController, error) {
	fc := &followerController{
		shardId:     shardId,
		epoch:       0,
		commitIndex: EntryId{},
		headIndex:   EntryId{},
		status:      NotMember,
		wal:         wal,
		closing:     false,
		log: log.With().
			Str("component", "follower-controller").
			Uint32("shard", shardId).
			Logger(),
	}

	if db, err := kv.NewDB(shardId, kvFactory); err != nil {
		return nil, err
	} else {
		fc.db = db
	}

	entryId, err := wal.GetHighestEntryOfEpoch(MaxEpoch)
	if err != nil {
		return nil, err
	}
	fc.headIndex = entryId

	fc.log.Info().
		Interface("head-index", fc.headIndex).
		Msg("Created follower")
	return fc, nil
}

func (fc *followerController) Close() error {
	if err := fc.wal.Close(); err != nil {
		return err
	}

	if err := fc.db.Close(); err != nil {
		return err
	}

	fc.log.Info().Msg("Closed follower")
	return nil
}

func (fc *followerController) Status() Status {
	fc.Lock()
	defer fc.Unlock()
	return fc.status
}

func (fc *followerController) Epoch() uint64 {
	fc.Lock()
	defer fc.Unlock()
	return fc.epoch
}

func (fc *followerController) Fence(req *proto.FenceRequest) (*proto.FenceResponse, error) {
	fc.Lock()
	defer fc.Unlock()

	if err := checkEpochLaterIn(req, fc.epoch); err != nil {
		return nil, err
	}

	fc.epoch = req.GetEpoch()
	fc.status = Fenced
	return &proto.FenceResponse{
		Epoch:     fc.epoch,
		HeadIndex: fc.headIndex.toProto(),
	}, nil
}

func (fc *followerController) Truncate(req *proto.TruncateRequest) (*proto.TruncateResponse, error) {
	fc.Lock()
	defer fc.Unlock()

	if err := checkStatus(Fenced, fc.status); err != nil {
		return nil, err
	}
	if err := checkEpochEqualIn(req, fc.epoch); err != nil {
		return nil, err
	}

	fc.status = Follower
	fc.epoch = req.Epoch
	headEntryId, err := fc.wal.TruncateLog(EntryIdFromProto(req.HeadIndex))
	if err != nil {
		return nil, err
	}
	fc.headIndex = headEntryId

	return &proto.TruncateResponse{
		Epoch:     req.Epoch,
		HeadIndex: headEntryId.toProto(),
	}, nil
}

func (fc *followerController) AddEntries(stream proto.OxiaLogReplication_AddEntriesServer) error {
	for {
		if addEntryReq, err := stream.Recv(); err != nil {
			return err
		} else if res, err := fc.addEntry(addEntryReq); err != nil {
			return err
		} else if err = stream.Send(res); err != nil {
			return err
		}
	}
}

func (fc *followerController) addEntry(req *proto.AddEntryRequest) (*proto.AddEntryResponse, error) {
	fc.Lock()
	defer fc.Unlock()

	if fc.status != Follower && fc.status != Fenced {
		return nil, errors.Wrapf(ErrorInvalidStatus, "AddEntry request when status = %+v", fc.status)
	}
	if req.GetEpoch() < fc.epoch {
		/*
		 A follower node rejects an entry from the leader.


		  If the leader has a lower epoch than the follower then the
		  follower must reject it with an INVALID_EPOCH response.

		  Key points:
		  - The epoch of the response should be the epoch of the
		    request so that the leader will not ignore the response.
		*/
		return &proto.AddEntryResponse{
			Epoch:        req.Epoch,
			EntryId:      nil,
			InvalidEpoch: true,
		}, nil
	}

	// A follower node confirms an entry to the leader
	//
	// The follower adds the entry to its log, sets the head index
	// and updates its commit index with the commit index of
	// the request.
	fc.status = Follower
	fc.epoch = req.Epoch
	if err := fc.wal.Append(req.GetEntry()); err != nil {
		return nil, err
	}

	fc.headIndex = EntryIdFromProto(req.Entry.EntryId)
	oldCommitIndex := fc.commitIndex
	fc.commitIndex = EntryIdFromProto(req.CommitIndex)

	err := fc.wal.ReadSync(oldCommitIndex, fc.commitIndex, func(entry *proto.LogEntry) error {
		br := &proto.WriteRequest{}
		if err := pb.Unmarshal(entry.Value, br); err != nil {
			return err
		}

		_, err := fc.db.ProcessWrite(br)
		return err
	})

	if err != nil {
		return nil, err
	}
	return &proto.AddEntryResponse{
		Epoch:        fc.epoch,
		EntryId:      req.Entry.EntryId,
		InvalidEpoch: false,
	}, nil

}

type MessageWithEpoch interface {
	GetEpoch() uint64
}

func checkEpochLaterIn(req MessageWithEpoch, expected uint64) error {
	if req.GetEpoch() <= expected {
		return errors.Wrapf(ErrorInvalidEpoch, "Got old epoch %d, when at %d", req.GetEpoch(), expected)
	}
	return nil
}

func checkEpochEqualIn(req MessageWithEpoch, expected uint64) error {
	if req.GetEpoch() != expected {
		return errors.Wrapf(ErrorInvalidEpoch, "Got clashing epoch %d, when at %d", req.GetEpoch(), expected)
	}
	return nil
}

func checkStatus(expected, actual Status) error {
	if actual != expected {
		return errors.Wrapf(ErrorInvalidStatus, "Received message in the wrong state. In %+v, should be %+v.", actual, expected)
	}
	return nil
}