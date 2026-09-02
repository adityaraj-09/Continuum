#include <stdio.h>
#include <stdlib.h>

#include "engine.h"

static void fail(const char *msg)
{
	fprintf(stderr, "FAIL: %s\n", msg);
	exit(1);
}

int main(void)
{
	struct book b;
	struct md_event ev = {0};
	struct order_intent in;
	uint32_t seq = 0;
	int32_t bid, ask;
	uint32_t bid_sz, ask_sz;

	book_init(&b);

	ev.type = MD_ADD;
	ev.side = SIDE_BID;
	ev.instrument = 1;
	ev.price = 100;
	ev.size = 10;
	ev.seq = 1;
	book_apply(&b, &ev);
	if (strategy_decide(&b, &seq, &in))
		fail("intent before ask exists");

	ev.type = MD_ADD;
	ev.side = SIDE_ASK;
	ev.price = 103;
	ev.size = 10;
	ev.seq = 2;
	book_apply(&b, &ev);

	if (!book_top(&b, &bid, &bid_sz, &ask, &ask_sz))
		fail("expected two-sided book");
	if (bid != 100 || ask != 103 || bid_sz != 10 || ask_sz != 10)
		fail("top of book mismatch");
	if (!strategy_decide(&b, &seq, &in))
		fail("expected join-the-spread intent");
	if (in.side != SIDE_BUY || in.price != 101 || in.size != 1 ||
	    in.instrument != 1)
		fail("intent fields wrong");

	ev.type = MD_DELETE;
	ev.side = SIDE_BID;
	ev.price = 100;
	ev.size = 0;
	ev.seq = 3;
	book_apply(&b, &ev);
	if (strategy_decide(&b, &seq, &in))
		fail("intent after bid deleted");

	printf("PASS: parser/book/strategy tape\n");
	return 0;
}
