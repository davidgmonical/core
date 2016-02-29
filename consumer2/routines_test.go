package consumer

import (
	"encoding/json"
	"io/ioutil"
	"math/rand"
	"os"
	"testing"

	etcd "github.com/coreos/etcd/client"
	gc "github.com/go-check/check"
	dbTuple "github.com/pippio/api-server/database"
	"github.com/stretchr/testify/mock"
	rocks "github.com/tecbot/gorocksdb"

	"github.com/pippio/consensus"
	"github.com/pippio/gazette/journal"
	"github.com/pippio/gazette/recoverylog"
	"github.com/pippio/gazette/topic"
)

type RoutinesSuite struct{}

func (s *RoutinesSuite) TestShardName(c *gc.C) {
	c.Check(shardName(42), gc.Equals, "shard-042")
}

func (s *RoutinesSuite) TestHintsPath(c *gc.C) {
	c.Check(hintsPath(s.treeFixture().Key, 42), gc.Equals, "/foo/hints/shard-042")
}

func (s *RoutinesSuite) TestLoadHints(c *gc.C) {
	runner := &Runner{RecoveryLogRoot: "path/to/recovery/logs/"}

	// Expect valid hints are found & loaded.
	hints, err := loadHints(12, runner, s.treeFixture())
	c.Check(err, gc.IsNil)
	c.Check(hints, gc.DeepEquals, s.hintsFixture())

	// Malformed hints.
	hints, err = loadHints(30, runner, s.treeFixture())
	c.Check(err, gc.ErrorMatches, "invalid character .*")

	// Missing hints.
	hints, err = loadHints(8, runner, s.treeFixture())
	c.Check(err, gc.IsNil)
	c.Check(hints, gc.DeepEquals, recoverylog.FSMHints{
		LogMark: journal.NewMark("path/to/recovery/logs/shard-008", -1),
	})
}

func (s *RoutinesSuite) TestStoreHints(c *gc.C) {
	var mockKeys consensus.MockKeysAPI
	var calledSet = make(chan struct{})

	// Expect an async call to mockKeys.Set() passing encoded hints.
	match := mock.MatchedBy(func(h string) bool {
		var recovered recoverylog.FSMHints
		c.Check(json.Unmarshal([]byte(h), &recovered), gc.IsNil)
		return c.Check(recovered, gc.DeepEquals, s.hintsFixture())
	})
	mockKeys.On("Set", mock.Anything, "/a/hints/path", match, (*etcd.SetOptions)(nil)).
		Return(nil, nil).Run(func(mock.Arguments) { close(calledSet) })

	c.Check(storeHints(&mockKeys, s.hintsFixture(), "/a/hints/path"), gc.IsNil)
	<-calledSet
}

func (s *RoutinesSuite) TestLoadOffsetsFromEtcd(c *gc.C) {
	offsets, err := loadOffsetsFromEtcd(s.treeFixture())
	c.Check(err, gc.IsNil)

	c.Check(offsets, gc.DeepEquals, map[journal.Name]int64{
		"journal/part-001":       42,
		"journal/part-002":       43,
		"other-journal/part-002": 44,
	})

	offsets, err = loadOffsetsFromEtcd(&etcd.Node{Key: "/foo", Dir: true})
	c.Check(err, gc.IsNil)
	c.Check(offsets, gc.IsNil)

	badTree := s.treeFixture()
	badTree.Nodes[1].Nodes[1].Nodes[0].Value = "invalid" // other-journal/part-002.

	offsets, err = loadOffsetsFromEtcd(badTree)
	c.Check(err, gc.ErrorMatches, "strconv.ParseInt: .*")
}

func (s *RoutinesSuite) TestLoadAndStoreOffsetsToDB(c *gc.C) {
	path, err := ioutil.TempDir("", "routines-suite")
	c.Assert(err, gc.IsNil)
	defer func() { c.Check(os.RemoveAll(path), gc.IsNil) }()

	options := rocks.NewDefaultOptions()
	options.SetCreateIfMissing(true)
	defer options.Destroy()

	db, err := rocks.OpenDb(options, path)
	c.Assert(err, gc.IsNil)
	defer db.Close()

	wb := rocks.NewWriteBatch()
	wo := rocks.NewDefaultWriteOptions()
	ro := rocks.NewDefaultReadOptions()
	defer func() {
		wb.Destroy()
		wo.Destroy()
		ro.Destroy()
	}()

	offsets := map[journal.Name]int64{
		"journal/part-001":       42,
		"journal/part-002":       43,
		"other-journal/part-003": 44,
	}
	storeOffsets(wb, offsets)
	clearOffsets(offsets)
	c.Check(db.Write(wo, wb), gc.Equals, nil)

	// Expect |offsets| were Put to |wb| and then cleared.
	c.Check(wb.Count(), gc.Equals, 3)
	c.Check(offsets, gc.HasLen, 0)

	// Expect they're recovered from the database.
	recovered, err := loadOffsetsFromDB(db, ro)
	c.Check(err, gc.IsNil)
	c.Check(recovered, gc.DeepEquals, map[journal.Name]int64{
		"journal/part-001":       42,
		"journal/part-002":       43,
		"other-journal/part-003": 44,
	})

	// Test handling of a bad value encoding.
	cases := []struct {
		key           dbTuple.Tuple
		value, expect string
	}{
		// Unexpected key encodings.
		{dbTuple.Tuple{"_mark", "a/journal", "foo"}, "42", "bad DB mark length .*"},
		{dbTuple.Tuple{"_mark", "a/journal"}, "bad-value", "strconv.ParseInt: .*"},
		// Bad value encoding.
		{dbTuple.Tuple{"_mark", 0}, "42", "bad DB mark value .*"},
	}

	for _, tc := range cases {
		c.Check(db.Put(wo, tc.key.Pack(), []byte(tc.value)), gc.IsNil)
		_, err = loadOffsetsFromDB(db, ro)
		c.Check(err, gc.ErrorMatches, tc.expect)

		c.Check(db.Delete(wo, tc.key.Pack()), gc.IsNil) // Cleanup.
	}
}

func (s *RoutinesSuite) TestOffsetMerge(c *gc.C) {
	c.Check(mergeOffsets(
		map[journal.Name]int64{ // DB offsets.
			"journal/part-001": 100,
			"journal/part-002": 200,
			"journal/db-only":  300,
		},
		map[journal.Name]int64{ // Etcd offsets.
			"journal/part-001":  200,
			"journal/part-002":  100,
			"journal/etcd-only": 400,
		}), gc.DeepEquals,
		map[journal.Name]int64{
			"journal/db-only":   300,
			"journal/etcd-only": 400,
			"journal/part-001":  100, // DB is lower than Etcd, but DB wins.
			"journal/part-002":  200,
		})
}

func (s *RoutinesSuite) TestTopicShardMapping(c *gc.C) {
	foo := &topic.Description{Name: "foo", Partitions: 1}
	bar := &topic.Description{Name: "bar", Partitions: 4}
	baz := &topic.Description{Name: "baz", Partitions: 16}

	var topics [3]*topic.Description
	for i, j := range rand.Perm(len(topics)) {
		topics[i] = []*topic.Description{foo, bar, baz}[j]
	}

	n, err := numShards(topics[:])
	c.Check(n, gc.Equals, 16)
	c.Check(err, gc.IsNil)

	c.Check(journalsForShard(topics[:], 5), gc.DeepEquals,
		map[journal.Name]*topic.Description{
			"foo/part-000": foo, // 5 % 2.
			"bar/part-001": bar, // 5 % 4.
			"baz/part-005": baz, // 5 % 16.
		})
	c.Check(journalsForShard(topics[:], 14), gc.DeepEquals,
		map[journal.Name]*topic.Description{
			"foo/part-000": foo, // 14 % 2.
			"bar/part-002": bar, // 14 % 4.
			"baz/part-014": baz, // 14 % 16.
		})

	// foo => 2 partitions. Expect it's still mappable.
	foo.Partitions = 2
	n, err = numShards(topics[:])
	c.Check(n, gc.Equals, 16)
	c.Check(err, gc.IsNil)

	c.Check(journalsForShard(topics[:], 7), gc.DeepEquals,
		map[journal.Name]*topic.Description{
			"foo/part-001": foo, // 7 % 2
			"bar/part-003": bar, // 7 % 4
			"baz/part-007": baz, // 7 % 16
		})

	// foo => 3 partitions. Expect it's an invalid configuration.
	foo.Partitions = 3
	_, err = numShards(topics[:])
	c.Check(err, gc.ErrorMatches, "topic partitions must be multiples of each other")
}

func (s *RoutinesSuite) treeFixture() *etcd.Node {
	shard012, _ := json.Marshal(s.hintsFixture())

	return &etcd.Node{
		Key: "/foo", Dir: true,
		Nodes: etcd.Nodes{
			{
				Key: "/foo/hints", Dir: true,
				Nodes: etcd.Nodes{
					{Key: "/foo/hints/shard-012", Value: string(shard012)},
					{Key: "/foo/hints/shard-030", Value: "... malformed ..."},
				},
			}, {
				Key: "/foo/offsets", Dir: true,
				Nodes: etcd.Nodes{
					{
						Key: "/foo/offsets/journal", Dir: true,
						Nodes: etcd.Nodes{
							{Key: "/foo/offsets/journal/part-001", Value: "2a"},
							{Key: "/foo/offsets/journal/part-002", Value: "2b"},
						},
					},
					{
						Key: "/foo/offsets/other-journal", Dir: true,
						Nodes: etcd.Nodes{
							{Key: "/foo/offsets/other-journal/part-002", Value: "2c"},
						},
					},
				},
			},
		},
	}
}

func (s *RoutinesSuite) hintsFixture() recoverylog.FSMHints {
	return recoverylog.FSMHints{
		LogMark:       journal.Mark{Journal: "some/recovery/logs/shard-012", Offset: 1234},
		FirstChecksum: 1212123,
		FirstSeqNo:    45645,
		Recorders:     []recoverylog.RecorderRange{{ID: 123, LastSeqNo: 456}},
		SkipWrites:    []recoverylog.Fnode{42, 46},
		Properties:    map[string]string{"foo": "bar"},
	}
}

var _ = gc.Suite(&RoutinesSuite{})

func Test(t *testing.T) { gc.TestingT(t) }