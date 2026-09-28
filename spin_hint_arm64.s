#include "textflag.h"

// func spinHint()
//
// YIELD is the arm64 spin-wait hint, repeated eight times to mirror spin_hint_amd64.s. Unmeasured: YIELD may retire
// almost immediately on some cores (Apple M-series), making this a much shorter wait than eight PAUSEs.
TEXT ·spinHint(SB), NOSPLIT, $0-0
	MOVD $8, R0
yield:
	YIELD
	SUB $1, R0
	CBNZ R0, yield
	RET
