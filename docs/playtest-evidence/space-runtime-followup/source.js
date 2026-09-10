await controller.hold({buttons:['back']},100);
checkpoint('reset',await observe());
await controller.hold({rt:0.5,lx:0.5},1000);
checkpoint('controller-flight',await observe());
await keyboard.hold(['W'],500);
checkpoint('keyboard-thrust',await observe());
