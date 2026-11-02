// JNI side of EchoCanceller.java: SpeexDSP's echo canceller, then its preprocessor for the
// residual echo and noise. One state per call of create; frames of a fixed size.
#include <jni.h>
#include <stdlib.h>

#include "speex/speex_echo.h"
#include "speex/speex_preprocess.h"

typedef struct {
    SpeexEchoState *echo;
    SpeexPreprocessState *pre;
    int frame;
} aec;

JNIEXPORT jlong JNICALL
Java_dev_jarvis_shared_EchoCanceller_create(JNIEnv *env, jclass cls, jint frame, jint tail, jint rate) {
    aec *a = calloc(1, sizeof(aec));
    a->frame = frame;
    a->echo = speex_echo_state_init(frame, tail);
    speex_echo_ctl(a->echo, SPEEX_ECHO_SET_SAMPLING_RATE, &rate);
    a->pre = speex_preprocess_state_init(frame, rate);
    speex_preprocess_ctl(a->pre, SPEEX_PREPROCESS_SET_ECHO_STATE, a->echo);
    int on = 1, off = 0;
    int suppress = -45, active = -20, noise = -20;
    speex_preprocess_ctl(a->pre, SPEEX_PREPROCESS_SET_DENOISE, &on);
    speex_preprocess_ctl(a->pre, SPEEX_PREPROCESS_SET_NOISE_SUPPRESS, &noise);
    speex_preprocess_ctl(a->pre, SPEEX_PREPROCESS_SET_ECHO_SUPPRESS, &suppress);
    speex_preprocess_ctl(a->pre, SPEEX_PREPROCESS_SET_ECHO_SUPPRESS_ACTIVE, &active);
    speex_preprocess_ctl(a->pre, SPEEX_PREPROCESS_SET_AGC, &off);
    speex_preprocess_ctl(a->pre, SPEEX_PREPROCESS_SET_DEREVERB, &off);
    return (jlong) (intptr_t) a;
}

// process: mic and ref (what played as the mic heard it) to out, each a frame long.
JNIEXPORT void JNICALL
Java_dev_jarvis_shared_EchoCanceller_process(JNIEnv *env, jclass cls, jlong h, jshortArray mic, jshortArray ref,
                                             jshortArray out) {
    aec *a = (aec *) (intptr_t) h;
    jshort *m = (*env)->GetShortArrayElements(env, mic, NULL);
    jshort *r = (*env)->GetShortArrayElements(env, ref, NULL);
    jshort *o = (*env)->GetShortArrayElements(env, out, NULL);
    speex_echo_cancellation(a->echo, m, r, o);
    speex_preprocess_run(a->pre, o);
    (*env)->ReleaseShortArrayElements(env, out, o, 0);
    (*env)->ReleaseShortArrayElements(env, ref, r, JNI_ABORT);
    (*env)->ReleaseShortArrayElements(env, mic, m, JNI_ABORT);
}

JNIEXPORT void JNICALL
Java_dev_jarvis_shared_EchoCanceller_reset(JNIEnv *env, jclass cls, jlong h) {
    aec *a = (aec *) (intptr_t) h;
    speex_echo_state_reset(a->echo);
}

JNIEXPORT void JNICALL
Java_dev_jarvis_shared_EchoCanceller_destroy(JNIEnv *env, jclass cls, jlong h) {
    aec *a = (aec *) (intptr_t) h;
    speex_preprocess_state_destroy(a->pre);
    speex_echo_state_destroy(a->echo);
    free(a);
}
