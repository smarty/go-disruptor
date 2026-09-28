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
