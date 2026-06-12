
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

## ⚠️ Dependencies


this program uses `zenity` and `xclip`, thus these two will be installed on your computer when installing jigedit.

to remove those dependencies after uninstalling this program, run: 


⚠️ ⚠️ ⚠️  **warning!** remove these two at your own risk! 

**Ubuntu / Debian:**
```bash
sudo apt-get remove --autoremove zenity xclip
```

**Arch Linux:**
```bash.
sudo pacman -Rns zenity xclip
```

**Fedora / RHEL:**
```bash
sudo dnf remove zenity xclip
sudo dnf autoremove
```
