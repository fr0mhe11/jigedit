
# JigEdit

"A Sane Editor For The Sane People"

As the discription implies, **jigedit** aims to be a simple and modern terminal-based text editor  that can be picked up effortlessly thanks to its almost non-existent learning curve.



![image](https://github.com/fr0mhe11/jigedit/blob/main/images/jigbanner2.png?raw=true)

heres a picture of jigedit editing it's own code

![image](https://github.com/fr0mhe11/jigedit/blob/main/images/jigscreen2.png?raw=true)



## Features

- NotePad-like shortcuts 
- extremely lightweight
- enter current time by pressing F5 (just like NotePad!)
- out-of-the-box experience : no need to spend eternity editing configs or learning new shortcuts
- multi-tab management
- command palette
- **uses system file picker for managing files:** no more spening most of your time writing file PATHs 

- intuitive terminal commands




## Installation

also install `wl-clipboard` if you are on wayland for better clipboard features   

### Install jigedit on linux **(recommended)** :

```bash
curl -sL https://raw.githubusercontent.com/fr0mhe11/jigedit/main/install.sh | bash
```

### install via ```go install``` :

```bash
go install github.com/fr0mhe11/jigedit@latest
```


### to uninstall (curl)

```bash
# 1. remove program file
sudo rm /usr/local/bin/jigedit

# 2. remove settings file (optional)
rm -rf ~/.config/jigedit
```


⚠️ Dependencies

This program uses `zenity` (for file dialogs), and either `wl-clipboard` (for Wayland) or `xclip` (for X11) for clipboard support. Depending on your environment, these will be installed on your computer when installing jigedit.

To remove those dependencies after uninstalling this program, run the following commands:

⚠️ ⚠️ ⚠️ Warning! Remove these at your own risk! (Other programs on your system might be using them)

**Ubuntu / Debian:**
```bash
sudo apt-get remove --autoremove zenity xclip wl-clipboard
```

**Arch Linux:**
```bash
sudo pacman -Rns zenity xclip wl-clipboard
```

**Fedora / RHEL:**
```bash
sudo dnf remove zenity xclip wl-clipboard
sudo dnf autoremove
```




## TODO
- [x] extensive encoding support
- [x] multi cursor feature

- [ ] make selection behave like micro/kwrite?
- [ ] syntex highlighting
- [ ] atomic file save

(still deciding whether to implement the last 2 features)

