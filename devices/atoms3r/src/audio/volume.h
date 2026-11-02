#pragma once

// Speaker volume in percent (0..100). The codec shares Wire1 with LEDs and touch, which belong
// to the main loop: set() and change() only from there. Other tasks use request().
namespace volume {

int level();
void set(int percent);  // shows the LED level bar
void change(int step);

// Thread-safe, applied by update(). A non-zero step wins over level (level < 0 = none): the
// model tends to send both ("leiser" as level 0 with step -10). Returns the new volume.
int request(int level, int step);
void update();

}  // namespace volume
