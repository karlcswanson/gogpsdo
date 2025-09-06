# gogpsdo - Chrony Driver for HP Z3805A GPSDO


## Chrony Config Notes
```
refclock SOCK /var/run/chrony/gpsdo.sock refid GPSD stratum 1 prefer

refclock PHC /dev/ptp0:extpps poll 0 refid PPSG lock GPSD
# OR
refclock PPS /dev/pps0 refid PPSG lock GPSD poll 2
```

## CM4 PPS/PHC & RTC Config
`/boot/firmware/config.txt`


```
# RTC
dtparam=i2c_vc=on
dtoverlay=i2c-rtc,pcf85063a,i2c_csi_dsi
# PPS0
dtoverlay=pps-gpio,gpiopin=18
# PPS1
dtoverlay=pps-gpio,gpiopin=21,devicename=pps1
```


### Verify PPS Functionality
```
pi@cm4:~/gogpsdo $ sudo ppstest /dev/pps0
trying PPS source "/dev/pps0"
found PPS source "/dev/pps0"
ok, found 1 source(s), now start fetching data...
source 0 - assert 1757196686.001064847, sequence: 74527 - clear  0.000000000, sequence: 0
source 0 - assert 1757196687.001070380, sequence: 74528 - clear  0.000000000, sequence: 0
source 0 - assert 1757196688.001069746, sequence: 74529 - clear  0.000000000, sequence: 0
source 0 - assert 1757196689.001069279, sequence: 74530 - clear  0.000000000, sequence: 0
```