#ifndef CONTINUUM_MD_H
#define CONTINUUM_MD_H

#include <stdint.h>

#define MD_MAGIC	0x4D443031u	/* "MD01" */
#define ORDER_MAGIC	0x4F523031u	/* "OR01" */

#define MD_ADD		1
#define MD_MODIFY	2
#define MD_DELETE	3
#define MD_TRADE	4

#define SIDE_BID	0
#define SIDE_ASK	1
#define SIDE_BUY	0
#define SIDE_SELL	1

#define BOOK_LEVELS	8
#define ORDER_PORT	9002

struct md_msg {
	uint32_t magic;
	uint16_t type;
	uint16_t side;
	uint16_t instrument;
	uint16_t _pad;
	int32_t price;
	uint32_t size;
	uint32_t seq;
} __attribute__((packed));

struct order_msg {
	uint32_t magic;
	uint16_t side;
	uint16_t instrument;
	int32_t price;
	uint32_t size;
	uint32_t seq;
} __attribute__((packed));

struct md_event {
	uint16_t type;
	uint16_t side;
	uint16_t instrument;
	int32_t price;
	uint32_t size;
	uint32_t seq;
};

struct order_intent {
	uint16_t side;
	uint16_t instrument;
	int32_t price;
	uint32_t size;
	uint32_t seq;
};

struct book_level {
	int32_t price;
	uint32_t size;
};

struct book_side {
	struct book_level lv[BOOK_LEVELS];
	int n;
};

struct book {
	uint16_t instrument;
	struct book_side bid;
	struct book_side ask;
	uint32_t applies;
};

struct stamp {
	uint64_t n;
	uint64_t sum_ns;
	uint64_t min_ns;
	uint64_t max_ns;
};

struct reply_path {
	uint8_t local_mac[6];
	uint8_t remote_mac[6];
	uint32_t local_ip;
	uint32_t remote_ip;
	uint16_t local_port;
	int valid;
};

#define ORDER_FRAME_LEN \
	(14 + 20 + 8 + (int)sizeof(struct order_msg))

#endif
