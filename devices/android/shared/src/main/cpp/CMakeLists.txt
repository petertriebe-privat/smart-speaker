# The phone's echo canceller (#75): SpeexDSP's echo canceller (mdf.c) and preprocessor (residual
# echo and noise) from speexdsp-1.2.1 (Xiph.org, BSD, speexdsp/COPYING), only the files they
# need, floating point with KISS FFT; jarvis_aec.c is the JNI side (EchoCanceller.java).
cmake_minimum_required(VERSION 3.22.1)
project(jarvis_aec C)

add_library(jarvis_aec SHARED
    jarvis_aec.c
    speexdsp/mdf.c
    speexdsp/preprocess.c
    speexdsp/filterbank.c
    speexdsp/fftwrap.c
    speexdsp/kiss_fft.c
    speexdsp/kiss_fftr.c)
target_include_directories(jarvis_aec PRIVATE speexdsp speexdsp/include)
target_compile_definitions(jarvis_aec PRIVATE FLOATING_POINT USE_KISS_FFT EXPORT=)
target_compile_options(jarvis_aec PRIVATE -O2 -w)
target_link_libraries(jarvis_aec m log)
# 16 KB pages (Android 15+ devices may use them; the phone warned about the 4 KB alignment)
target_link_options(jarvis_aec PRIVATE "-Wl,-z,max-page-size=16384")
