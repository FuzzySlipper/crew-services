await input([{kind:'hold',keys:[123],ms:100}]);
await input([{kind:'point',x:640,y:460,width:1280,height:720},{kind:'click',button:1,ms:100}]);
await keyboard.hold(['Tab'],100);
checkpoint('focused-baseline',await observe());
await controller.hold({rx:0.7,ly:0.5},1000);
checkpoint('controller-look-move',await observe());
await keyboard.hold(['W'],700);
checkpoint('keyboard-forward',await observe());
