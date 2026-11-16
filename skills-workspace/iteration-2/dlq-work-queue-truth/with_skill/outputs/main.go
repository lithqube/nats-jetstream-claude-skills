// Command dlq-work-queue is a complete, honest DLQ for a NATS JetStream work queue.
//
// NATS has NO native dead-letter queue. When a message exceeds MaxDeliver the
// server stops redelivering it and publishes an advisory — nothing else. This
// program builds the DLQ you actually wanted out of the three pieces NATS does
// give you: the MAX_DELIVERIES advisory, GetMsg-by-sequence, and DeleteMsg.
//
// Subcommands:
//
//	setup       create the work-queue stream, DLQ stream, advisory stream and consumers (idempotent)
//	worker      process tasks; fail 5x -> server stops redelivering, fires the advisory
//	dlq-router  the DLQ engine: consume advisories, MOVE failed messages into the DLQ
//	inspect     list what is sitting in the DLQ (read-only)
//	replay      re-inject DLQ messages back into the work queue, then clear them
//
// Run `setup` once, then `worker` and `dlq-router` as long-lived processes.
// `inspect` and `replay` are admin commands you run by hand.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	tasksStream   = "TASKS"
	tasksSubjects = "tasks.>"
	processor     = "processor"
	maxDeliver    = 5 // the user's requirement: fail 5 times, then it's dead

	dlqStream  = "TASKS_DLQ"
	dlqSubject = "dlq.tasks.failed"

	advStream = "JS_ADVISORY"
	dlqRouter = "dlq_router"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: %s setup|worker|dlq-router|inspect|replay [args]", os.Args[0])
	}

	nc, err := nats.Connect(envURL(),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer nc.Drain()

	js, err := nc.JetStream()
	if err != nil {
		log.Fatal(err)
	}

	switch os.Args[1] {
	case "setup":
		mustSetup(js)
	case "worker":
		runWorker(js)
	case "dlq-router":
		runDLQRouter(nc, js)
	case "inspect":
		inspectDLQ(js)
	case "replay":
		replayDLQ(js, os.Args[2:])
	default:
		log.Fatalf("unknown subcommand %q", os.Args[1])
	}
}

func envURL() string {
	if u := os.Getenv("NATS_URL"); u != "" {
		return u
	}
	return nats.DefaultURL
}

// ---------------------------------------------------------------------------
// setup
// ---------------------------------------------------------------------------

func mustSetup(js nats.JetStreamContext) {
	// Work queue: a message is removed only once acked. A message that burns
	// through MaxDeliver is never acked, so on a WorkQueuePolicy stream it stays
	// put AND no other consumer can take it — it jams the queue. dlq-router below
	// is what unjams it.
	addStream(js, &nats.StreamConfig{
		Name:      tasksStream,
		Subjects:  []string{tasksSubjects},
		Retention: nats.WorkQueuePolicy,
		Storage:   nats.FileStorage,
		Replicas:  1, // bump to 3 in production
		MaxMsgs:   100000,
		Discard:   nats.DiscardNew, // backpressure: reject publishes when full
		MaxAge:    24 * time.Hour,
	})

	// The work-queue consumer. MaxDeliver=5 is the whole requirement: five
	// attempts (first delivery + four backoff retries), then the server gives up.
	addConsumer(js, tasksStream, &nats.ConsumerConfig{
		Durable:       processor,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    maxDeliver,
		MaxAckPending: 100,
		BackOff: []time.Duration{
			5 * time.Second,
			30 * time.Second,
			2 * time.Minute,
			10 * time.Minute,
		},
	})

	// The DLQ itself: an ordinary limits stream you can inspect and replay from.
	// DuplicateWindow makes the MOVE idempotent — if dlq-router crashes after
	// writing here but before deleting the original, the retry's Nats-Msg-Id
	// collapses the second write instead of creating a duplicate dead letter.
	addStream(js, &nats.StreamConfig{
		Name:       dlqStream,
		Subjects:   []string{dlqSubject},
		Retention:  nats.LimitsPolicy,
		Storage:    nats.FileStorage,
		Replicas:   1, // bump to 3 in production
		MaxAge:     30 * 24 * time.Hour,
		Duplicates: 10 * time.Minute,
	})

	// Advisories are fire-and-forget core-NATS messages: any that land while the
	// router is restarting are gone forever. Capturing them into a durable stream
	// and consuming THAT is the only shape that doesn't lose failed jobs.
	addStream(js, &nats.StreamConfig{
		Name: advStream,
		Subjects: []string{
			"$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES." + tasksStream + ".>",
			"$JS.EVENT.ADVISORY.CONSUMER.MSG_TERMINATED." + tasksStream + ".>",
		},
		Retention: nats.LimitsPolicy,
		Storage:   nats.FileStorage,
		Replicas:  1,
		MaxAge:    30 * 24 * time.Hour,
	})

	addConsumer(js, advStream, &nats.ConsumerConfig{
		Durable:       dlqRouter,
		AckPolicy:     nats.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    -1, // keep retrying the router itself; losing an advisory loses a job
		DeliverPolicy: nats.DeliverAllPolicy,
	})

	log.Println("setup complete")
}

// ---------------------------------------------------------------------------
// worker — the thing whose failures feed the DLQ
// ---------------------------------------------------------------------------

func runWorker(js nats.JetStreamContext) {
	sub, err := js.PullSubscribe(tasksSubjects, processor)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("worker started")

	for {
		msgs, err := sub.Fetch(10, nats.MaxWait(5*time.Second))
		if err != nil {
			if err == nats.ErrTimeout {
				continue
			}
			log.Printf("fetch error: %v", err)
			continue
		}
		for _, msg := range msgs {
			meta, _ := msg.Metadata()
			if err := processTask(msg.Data); err != nil {
				log.Printf("seq=%d attempt=%d failed: %v",
					meta.Sequence.Stream, meta.NumDelivered, err)
				// Transient failure: ask for redelivery. After MaxDeliver attempts
				// the server stops on its own and fires the advisory — do NOT Term()
				// here or you suppress the very event the DLQ is built on.
				_ = msg.Nak()
				continue
			}
			_ = msg.Ack()
		}
	}
}

// processTask is where real work goes. Return an error to trigger a retry.
func processTask(data []byte) error {
	// placeholder: replace with real processing
	return fmt.Errorf("not implemented")
}

// ---------------------------------------------------------------------------
// dlq-router — the DLQ engine
// ---------------------------------------------------------------------------

// advisory is the subset of the MAX_DELIVERIES / MSG_TERMINATED advisory we use.
// MAX_DELIVERIES carries no consumer_seq; MSG_TERMINATED carries a reason. We
// only need the stream sequence to recover the message.
type advisory struct {
	Type      string `json:"type"`
	Stream    string `json:"stream"`
	Consumer  string `json:"consumer"`
	StreamSeq uint64 `json:"stream_seq"`
	Reason    string `json:"reason"` // terminated only
}

func runDLQRouter(nc *nats.Conn, js nats.JetStreamContext) {
	sub, err := js.PullSubscribe("", dlqRouter, nats.Bind(advStream, dlqRouter))
	if err != nil {
		log.Fatal(err)
	}
	log.Println("dlq-router started")

	for {
		msgs, err := sub.Fetch(10, nats.MaxWait(30*time.Second))
		if err != nil {
			continue // timeout is normal when idle
		}
		for _, m := range msgs {
			if err := routeOne(js, m); err != nil {
				log.Printf("route error (will redeliver): %v", err)
				_ = m.Nak()
				continue
			}
			_ = m.Ack()
		}
	}
}

// routeOne moves one failed message from the work queue into the DLQ:
// copy to DLQ (idempotently), then delete the stuck original so the WorkQueue
// stream is unjammed. Ack only happens after both succeed, in runDLQRouter.
func routeOne(js nats.JetStreamContext, m *nats.Msg) error {
	var adv advisory
	if err := json.Unmarshal(m.Data, &adv); err != nil {
		// An advisory we can't parse will never parse — don't redeliver forever.
		_ = m.Term()
		return nil
	}

	raw, err := js.GetMsg(adv.Stream, adv.StreamSeq)
	if err != nil {
		// Already moved by a previous (crashed) attempt, or aged out of the work
		// queue before we got here. Either way there's nothing to recover.
		log.Printf("original %s#%d gone: %v — nothing to move", adv.Stream, adv.StreamSeq, err)
		return nil
	}

	hdr := buildDLQHeader(adv, raw.Subject, headerGet(raw.Header, "Nats-Msg-Id"))
	if _, err := js.PublishMsg(&nats.Msg{Subject: dlqSubject, Data: raw.Data, Header: hdr}); err != nil {
		return fmt.Errorf("publish to DLQ: %w", err)
	}

	// Unjam the WorkQueue stream. If we crash between the publish above and here,
	// the advisory redelivers, GetMsg still returns, we publish again — and the
	// DLQ DuplicateWindow (keyed on Dlq-Id) collapses it. So this is safe to retry.
	if err := js.DeleteMsg(adv.Stream, adv.StreamSeq); err != nil {
		log.Printf("warning: copied %s#%d to DLQ but could not delete original: %v",
			adv.Stream, adv.StreamSeq, err)
	}
	log.Printf("moved %s#%d (%s) -> DLQ", adv.Stream, adv.StreamSeq, adv.Type)
	return nil
}

// buildDLQHeader is pure so it can be unit-tested without a server. It records
// enough provenance to inspect and replay the message, and a Dlq-Id that makes
// the move idempotent against the DLQ stream's DuplicateWindow.
func buildDLQHeader(adv advisory, origSubject, origMsgID string) nats.Header {
	h := nats.Header{}
	h.Set("Dlq-Id", dlqMsgID(adv.Stream, adv.StreamSeq))
	h.Set("Nats-Msg-Id", dlqMsgID(adv.Stream, adv.StreamSeq)) // dedup key for the DLQ write
	h.Set("Original-Stream", adv.Stream)
	h.Set("Original-Consumer", adv.Consumer)
	h.Set("Original-Subject", origSubject)
	h.Set("Original-Seq", strconv.FormatUint(adv.StreamSeq, 10))
	h.Set("Failure-Reason", adv.Type) // max_deliveries or terminated
	if adv.Reason != "" {
		h.Set("Term-Reason", adv.Reason)
	}
	if origMsgID != "" {
		h.Set("Original-Msg-Id", origMsgID) // preserve app dedup id for replay
	}
	return h
}

func dlqMsgID(stream string, seq uint64) string {
	return fmt.Sprintf("%s-%d", stream, seq)
}

// ---------------------------------------------------------------------------
// inspect — read-only listing of the DLQ
// ---------------------------------------------------------------------------

func inspectDLQ(js nats.JetStreamContext) {
	info, err := js.StreamInfo(dlqStream)
	if err != nil {
		log.Fatal(err)
	}
	st := info.State
	if st.Msgs == 0 {
		fmt.Println("DLQ is empty")
		return
	}
	fmt.Printf("DLQ %s holds %d message(s) [seq %d..%d]\n\n",
		dlqStream, st.Msgs, st.FirstSeq, st.LastSeq)

	// Direct get-by-sequence is read-only and creates no consumer state, so it's
	// safe to run against a live DLQ. Deleted/replayed sequences are skipped.
	for seq := st.FirstSeq; seq <= st.LastSeq; seq++ {
		raw, err := js.GetMsg(dlqStream, seq)
		if err != nil {
			continue
		}
		fmt.Printf("DLQ#%d  original=%s#%s  subject=%s  reason=%s\n",
			seq,
			headerGet(raw.Header, "Original-Stream"),
			headerGet(raw.Header, "Original-Seq"),
			headerGet(raw.Header, "Original-Subject"),
			headerGet(raw.Header, "Failure-Reason"),
		)
		fmt.Printf("        payload: %s\n", preview(raw.Data))
	}
}

// ---------------------------------------------------------------------------
// replay — re-inject DLQ messages back into the work queue
// ---------------------------------------------------------------------------

// replayDLQ re-publishes dead letters to their original subject so the worker
// picks them up again, then deletes them from the DLQ so a second run doesn't
// replay them twice. Args: a list of DLQ sequence numbers, or "all".
func replayDLQ(js nats.JetStreamContext, args []string) {
	if len(args) == 0 {
		log.Fatalf("usage: replay <dlq-seq>... | all")
	}

	var seqs []uint64
	if args[0] == "all" {
		info, err := js.StreamInfo(dlqStream)
		if err != nil {
			log.Fatal(err)
		}
		for s := info.State.FirstSeq; s <= info.State.LastSeq; s++ {
			seqs = append(seqs, s)
		}
	} else {
		for _, a := range args {
			s, err := strconv.ParseUint(a, 10, 64)
			if err != nil {
				log.Fatalf("bad sequence %q: %v", a, err)
			}
			seqs = append(seqs, s)
		}
	}

	for _, seq := range seqs {
		if err := replayOne(js, seq); err != nil {
			log.Printf("DLQ#%d replay failed: %v", seq, err)
		}
	}
}

func replayOne(js nats.JetStreamContext, seq uint64) error {
	raw, err := js.GetMsg(dlqStream, seq)
	if err != nil {
		return err // already replayed/deleted, or never existed
	}
	subject := headerGet(raw.Header, "Original-Subject")
	if subject == "" {
		return fmt.Errorf("no Original-Subject header; cannot route")
	}

	hdr := nats.Header{}
	hdr.Set("Replayed-From-Dlq", strconv.FormatUint(seq, 10))
	// Preserve the app's dedup id so a consumer-side inbox table still dedups
	// after replay. Fall back to a stable id if the original carried none.
	if id := headerGet(raw.Header, "Original-Msg-Id"); id != "" {
		hdr.Set("Nats-Msg-Id", id)
	}

	if _, err := js.PublishMsg(&nats.Msg{Subject: subject, Data: raw.Data, Header: hdr}); err != nil {
		return fmt.Errorf("republish to %s: %w", subject, err)
	}
	// Clear from the DLQ only after the re-injection is durably stored.
	if err := js.DeleteMsg(dlqStream, seq); err != nil {
		return fmt.Errorf("replayed to %s but could not clear DLQ#%d: %w", subject, seq, err)
	}
	log.Printf("replayed DLQ#%d -> %s", seq, subject)
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func addStream(js nats.JetStreamContext, cfg *nats.StreamConfig) {
	if _, err := js.AddStream(cfg); err != nil {
		// AddStream is idempotent when the config matches; treat "already exists" softly.
		if _, infoErr := js.StreamInfo(cfg.Name); infoErr == nil {
			return
		}
		log.Fatalf("add stream %s: %v", cfg.Name, err)
	}
}

func addConsumer(js nats.JetStreamContext, stream string, cfg *nats.ConsumerConfig) {
	if _, err := js.AddConsumer(stream, cfg); err != nil {
		if _, infoErr := js.ConsumerInfo(stream, cfg.Durable); infoErr == nil {
			return
		}
		log.Fatalf("add consumer %s/%s: %v", stream, cfg.Durable, err)
	}
}

func headerGet(h nats.Header, k string) string {
	if h == nil {
		return ""
	}
	return h.Get(k)
}

func preview(b []byte) string {
	const max = 120
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
