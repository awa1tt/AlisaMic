/* vqe_tap_udp.c — тап обработанного микрофона vqe_0 прямо в UDP на ПК.
 * Основано на vqe_tap.c из Alisa-ChatGPT-voice-assistant (rison1337), CC.
 * Перехватывает YandexVqeC_GetOutputChannelData (kind=6 = VQE, index=0 = vqe_0),
 * конвертирует float32 -> s16le и шлёт датаграммами по 160 сэмплов (10 мс).
 * Цель задаётся env ALICE_TAP_TARGET="192.168.3.5:5000".
 * Собирать: zig build-lib -O ReleaseSmall -dynamic -target arm-linux-gnueabihf.2.30 vqe_tap_udp.c
 */
typedef __SIZE_TYPE__ size_t;
extern void *dlsym(void *, const char *);
extern char *getenv(const char *);
extern int socket(int, int, int);
extern long sendto(int, const void *, size_t, int, const void *, unsigned int);
extern long write(int, const void *, size_t);
extern int *__errno_location(void);
extern void _exit(int);

struct inet_address { unsigned short family; unsigned short port; unsigned int addr; char zero[8]; };
static void *(*next_output)(void *, unsigned int, unsigned int, const float **, size_t *);
static struct inet_address address;
static unsigned int address_length;
static int tap_socket = -1, initialized;

static unsigned int parse_ip(const char *s) {
    unsigned int r = 0;
    for (int part = 0; part < 4; ++part) {
        unsigned int v = 0, digits = 0;
        while (*s >= '0' && *s <= '9') { v = v * 10 + (unsigned)(*s - '0'); ++s; ++digits; }
        if (!digits || v > 255) return 0;
        r |= v << (part * 8);       /* сетевой порядок: первый октет — младший байт LE-слова */
        if (part < 3 && *s == '.') ++s;
    }
    return r;
}
static unsigned short parse_port(const char *s) {
    unsigned int v = 0;
    while (*s >= '0' && *s <= '9') { v = v * 10 + (unsigned)(*s - '0'); ++s; }
    if (!v || v > 65535) return 0;
    return (unsigned short)((v >> 8) | (v << 8)); /* big-endian */
}

static void initialize(void) {
    initialized = 1;
    const char *t = getenv("ALICE_TAP_TARGET");
    if (!t || !t[0]) return;
    const char *colon = t;
    while (*colon && *colon != ':') ++colon;
    if (*colon != ':') return;
    unsigned int ip = parse_ip(t);
    unsigned short port = parse_port(colon + 1);
    if (!ip || !port) return;
    address.family = 2; /* AF_INET */
    address.port = port;
    address.addr = ip;
    address_length = sizeof(address);
    tap_socket = socket(2, 2 | 04000 | 02000000, 0); /* AF_INET, SOCK_DGRAM|NONBLOCK|CLOEXEC */
    if (tap_socket >= 0) {
        const char text[] = "ALICE_VQE_TAP_UDP channel=vqe_0 rate=16000 format=s16le\n";
        write(2, text, sizeof(text) - 1);
    }
}

void *YandexVqeC_GetOutputChannelData(void *engine, unsigned int kind,
                                    unsigned int index, const float **data,
                                    size_t *frames) {
    int entered_errno = *__errno_location();
    if (!next_output) next_output = dlsym((void *)(long)-1, "YandexVqeC_GetOutputChannelData");
    *__errno_location() = entered_errno;
    if (!next_output) _exit(2);
    void *error = next_output(engine, kind, index, data, frames);
    int original_errno = *__errno_location();
    if (!error && kind == 6 && index == 0 && data && frames && *data &&
        *frames > 0 && *frames <= 4096) {
        if (!initialized) initialize();
        if (tap_socket >= 0) {
            short packet[160];
            for (size_t offset = 0; offset < *frames;) {
                size_t count = *frames - offset;
                if (count > 160) count = 160;
                for (size_t i = 0; i < count; ++i) {
                    float value = (*data)[offset + i];
                    packet[i] = value != value ? 0 : value >= 1.0f ? 32767 :
                                value <= -1.0f ? -32768 : (short)(value * 32768.0f);
                }
                sendto(tap_socket, packet, count * sizeof(short), 0x40 | 0x4000,
                       &address, address_length);
                offset += count;
            }
        }
    }
    *__errno_location() = original_errno;
    return error;
}
