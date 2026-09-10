checkpoint('baseline',await observe());
await controller.hold({rx:0.7,ly:0.5},1000);
checkpoint('controller',await observe());
await keyboard.hold(['W'],700);
checkpoint('keyboard',await observe());
