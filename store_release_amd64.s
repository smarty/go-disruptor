//go:build amd64 && !race

#include "textflag.h"

// func storeRelease(address *atomic.Int64, value int64)
//
// A plain MOVQ is a release store on amd64 (TSO): it retires into the store buffer without waiting for ownership of
// the cache line, unlike the XCHG that sync/atomic's sequentially consistent Store compiles to. The compiler cannot
// move the caller's earlier stores past a call into assembly, and cannot inline it.
TEXT ·storeRelease(SB), NOSPLIT, $0-16
	MOVQ address+0(FP), AX
	MOVQ value+8(FP), BX
	MOVQ BX, (AX)
	RET

// func fullFence()
//
// A locked OR of zero on the top of the stack (the return address) is a full fence that changes no memory, and it
// touches only a line private to this goroutine. It is the Linux smp_mb idiom, and cheaper than MFENCE.
TEXT ·fullFence(SB), NOSPLIT, $0-0
	LOCK
	ORQ $0, 0(SP)
	RET
