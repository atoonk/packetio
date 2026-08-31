package packetio

// TxStats counts what one transmit queue has done. Counters are cumulative
// since the queue was opened and never reset.
type TxStats struct {
	// Packets and Bytes are what reached the NIC: frames accepted by Transmit,
	// and the sum of their lengths.
	Packets uint64
	Bytes   uint64

	// Completed is how many frames the NIC has finished sending and Complete
	// has reclaimed. Packets minus Completed is what the NIC still owns.
	Completed uint64

	// Batches is how many times a batch was published to the hardware: one
	// doorbell on mlx5, one kick or suppressed kick on AF_XDP. Packets divided
	// by Batches is the effective batch size.
	Batches uint64

	// Completions is how many completion events were consumed: completion queue
	// entries on mlx5, completion ring entries on AF_XDP. On a backend that
	// signals once per batch this is far smaller than Completed.
	Completions uint64

	// RingFull counts calls that could queue nothing because the transmit ring
	// had no free slots, and PoolEmpty calls that could allocate nothing
	// because every frame was in flight. Both mean the NIC is the limit.
	RingFull  uint64
	PoolEmpty uint64

	// Errors counts hardware or kernel errors reported for this queue:
	// error completions on mlx5, failed kicks on AF_XDP.
	Errors uint64

	// Backend holds counters only this backend has. Keys are lowercase and
	// stable within a backend, for example mlx5's "cqe_err" or AF_XDP's
	// "tx_invalid_descs".
	Backend map[string]uint64
}

// RxStats counts what one receive queue has done.
type RxStats struct {
	// Packets and Bytes are what was handed to the application by Receive.
	Packets uint64
	Bytes   uint64

	// Filled is how many frames were posted for the NIC to receive into.
	Filled uint64

	// Batches is how many times Receive asked the hardware and was given
	// something: one call that returned packets. Packets divided by Batches is
	// the effective receive batch size, and it is worth watching.
	//
	// A receive loop that looks busy while taking a fraction of the offered
	// load is usually taking it a handful of packets at a time, paying a
	// call's fixed cost over too few of them and leaving the card short of
	// posted buffers between bursts. Nothing else in this struct shows that:
	// packets, bytes and drops all look the same whether they arrived sixty
	// at a time or five. A backend once lost half its receive rate to exactly
	// that, invisibly, because this counter was not published.
	Batches uint64

	// Polls is how many times Poll was called, and Completions how many
	// completion events were consumed.
	Polls       uint64
	Completions uint64

	// PoolEmpty counts refills that posted nothing because no frame was free,
	// which is the application holding on to received frames too long.
	PoolEmpty uint64

	// Errors counts error completions and malformed descriptors.
	Errors uint64

	// Dropped is what the NIC or kernel discarded before the application saw
	// it, when the backend can report it. It is not always attributable to one
	// queue; a backend that cannot tell leaves it zero and reports the device
	// wide figure in Backend.
	Dropped uint64

	// Backend holds counters only this backend has.
	Backend map[string]uint64
}
