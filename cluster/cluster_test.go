package cluster_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/gigawattio/netlib"
	"github.com/gigawattio/testlib"
	"github.com/gigawattio/zklib/cluster"
	"github.com/gigawattio/zklib/cluster/primitives"
	"github.com/gigawattio/zklib/testutil"
)

var zkTimeout = 1 * time.Second

// ncc creates a new Coordinator for a given test cluster.
func ncc(t *testing.T, zkServers []string, data string, subscribers ...chan primitives.Update) *cluster.Coordinator {
	cc, err := cluster.NewCoordinator(zkServers, zkTimeout, "/"+testlib.CurrentRunningTest(), data, subscribers...)
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.Start(); err != nil {
		t.Fatal(err)
	}
	return cc
}

func TestClusterLeaderElection(t *testing.T) {
	// NB: tcSz == zookeeper test cluster size.
	for _, tcSz := range []int{1} {
		testutil.WithZk(t, tcSz, "127.0.0.1:2181", func(zkServers []string) {
			for _, sz := range []int{1, 2, 3, 4} {
				t.Logf("Testing with number of cluster members sz=%v", sz)

				members := make([]*cluster.Coordinator, sz)
				for i := 0; i < sz; i++ {
					cc := ncc(t, zkServers, fmt.Sprintf("i=%v", i))
					members[i] = cc

					go func(i int) {
						if err := cc.Stop(); err != nil {
							t.Fatalf("Stopping cc member #%v: %s", i, err)
						}

						wait := time.Duration(i*250) * time.Millisecond
						t.Logf("random wait for member=%s --> %s", cc.Id(), wait)
						time.Sleep(wait)

						if err := cc.Start(); err != nil {
							t.Fatalf("Starting cc member #%v: %s", i, err)
						}
					}(i)
				}

				time.Sleep(time.Duration(sz*600) * time.Millisecond)
				t.Logf("done sleeping")

				verifyState := func(replaceLeader bool) {
					var retried bool
				Retry:

					if len(members) == 0 {
						t.Logf("members was empty, returning early")
						return
					}

					var found *primitives.Node
					for _, member := range members {
						if leader := member.Leader(); leader != nil {
							found = leader
							break
						}
					}
					if found == nil {
						var reachable bool
						for _, zkServer := range zkServers {
							reachable = netlib.IsTcpPortReachable(zkServer)
							t.Logf("zkServer addr=%v is-reachable=%v", zkServer, reachable)
							if reachable {
								break
							}
						}
						if retried || !reachable {
							t.Fatalf("No leader found on any of the cluster nodes, is zookeeper running?")
						} else {
							log.Infof("Will retry state verification after waiting 1s")
							time.Sleep(1 * time.Second)
							retried = true
							goto Retry
						}
					}

					expectedLeaderStr := found.String()
					allMatch := true

					for _, member := range members {
						var leaderStr string
						if leader := member.Leader(); leader != nil {
							leaderStr = member.Leader().String()
						}
						t.Logf("%s thinks the leader is=/%s/", member.Id(), leaderStr)
						if leaderStr != expectedLeaderStr {
							t.Errorf("%s had leader=/%s/ but expected value=/%s/, caused allMatch=false", member.Id(), leaderStr, expectedLeaderStr)
							allMatch = false
						}
					}
					if !allMatch {
						t.Fatalf("not all cluster coordinators agreed on who the leader was")
					}

					if replaceLeader {
						for i, member := range members {
							if member.Mode() == primitives.Leader {
								if err := member.Stop(); err != nil {
									t.Fatal(err)
								}
								members[i] = ncc(t, zkServers, fmt.Sprintf("i=%v", i))
								t.Logf("Shut down leader member=%s and launched new one=%s", member.Id(), members[i].Id())
								break
							}
						}
					}
				}

				for i := 0; i < sz*2; i++ {
					t.Logf("iteration #%v tc_sz=%v members_sz=%v [ mutate ]----------------", i, len(zkServers), sz)
					verifyState(true)

					time.Sleep(100 * time.Millisecond)
					t.Logf("iteration #%v tc_sz=%v members_sz=%v [ verify ]----------------", i, len(zkServers), sz)
					verifyState(false)
				}

				for _, member := range members {
					if err := member.Stop(); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func Test_ClusterSubscriptions(t *testing.T) {
	testutil.WithZk(t, 1, "127.0.0.1:2181", func(zkServers []string) {
		var (
			subChan           = make(chan primitives.Update)
			lock              sync.Mutex
			numEventsReceived int
		)

		go func() {
			for {
				select {
				case updateInfo := <-subChan:
					log.Infof("New update=%+v", updateInfo)
					lock.Lock()
					numEventsReceived++
					lock.Unlock()
				}
			}
		}()

		cc := ncc(t, zkServers, "primary-cc", subChan)

		defer func() {
			if err := cc.Stop(); err != nil {
				t.Fatal(err)
			}
		}()

		time.Sleep(1 * time.Second)

		cc.Unsubscribe(subChan)

		if err := cc.Stop(); err != nil {
			t.Fatal(err)
		}

		lock.Lock()
		if numEventsReceived < 1 {
			t.Fatalf("Expected numEventsReceived >= 1, but actual numEventsReceived=%v", numEventsReceived)
		}
		prevNumEventsReceived := numEventsReceived
		lock.Unlock()

		if err := cc.Start(); err != nil {
			t.Fatal(err)
		}

		// Verify that unsubscribe works.

		time.Sleep(1 * time.Second)

		lock.Lock()
		if numEventsReceived != prevNumEventsReceived {
			t.Fatalf("Expected numEventsReceived to stay the same (was previously %v), but actual numEventsReceived=%v", prevNumEventsReceived, numEventsReceived)
		}
		lock.Unlock()
	})
}

func TestClusterMembersListing(t *testing.T) {
	for _, n := range []int{1, 2, 3, 5, 7, 11} {
		testutil.WithZk(t, 1, "127.0.0.1:2181", func(zkServers []string) {
			var (
				ccs             = []*cluster.Coordinator{}
				ready           = make(chan struct{})
				signalWhenReady = func(ch chan primitives.Update) {
					select {
					case <-ch:
						ready <- struct{}{}
					case <-time.After(zkTimeout):
						t.Fatalf("[n=%v] Timed out after %v waiting for ready signal", n, zkTimeout)
					}
				}
			)

			for i := 0; i < n; i++ {
				subChan := make(chan primitives.Update)
				go signalWhenReady(subChan)
				cc := ncc(t, zkServers, fmt.Sprintf("i=%v", i), subChan)
				<-ready
				ccs = append(ccs, cc)
				for j := 0; j < len(ccs); j++ {
					nodes, err := ccs[j].Members()
					if err != nil {
						t.Fatalf("[n=%v i=%v j=%v] %s", n, i, j, err)
					}
					if expected, actual := i+1, len(nodes); actual != expected {
						t.Fatalf("[n=%v i=%v j=%v] Expected number of members=%v but actual=%v; returned nodes=%+v", n, i, j, expected, actual, nodes)
					}
				}
			}

			for i := n - 1; i >= 0; i-- {
				if err := ccs[i].Stop(); err != nil {
					t.Fatalf("[n=%v i=%v] %s", n, i, err)
				}
				ccs = ccs[0 : len(ccs)-1]
				for j := 0; j < len(ccs); j++ {
					nodes, err := ccs[j].Members()
					if err != nil {
						t.Fatalf("[n=%v i=%v j=%v] %s", n, i, j, err)
					}
					if expected, actual := i, len(nodes); actual != expected {
						t.Fatalf("[n=%v i=%v j=%v] Expected number of members=%v but actual=%v; returned nodes=%+v", n, i, j, expected, actual, nodes)
					}
				}
			}
		})
	}
}
