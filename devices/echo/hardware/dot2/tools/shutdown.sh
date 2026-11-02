#!/bin/sh
sync
umount /data 2>/dev/null || mount -o remount,ro /data
sync
