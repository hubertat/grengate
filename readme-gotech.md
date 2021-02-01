# GOTech

Aplikacja TECH w passage.cl

## do czego służy

## tworzenie aplikacji

### kompilacja

Aby skomplilować roboczo:
```
make build
```

Aby skompilować wersje (darwin + linux + pliki statyczne):
```
make
```
Pliki pojawią się w folderze `./releases`


#### wersjonowanie GIT

Istotne jest aby przed kompilacją wersji produkcyjnej dodać kolejną wersję w tagu git:
```
git tag v0.x.y
git push origin --tags
```

Aby sprawdzić jakie wersje (tagi) istnieją: `git tag`.

Dlaczego jest to ważne? Bo *Makefile* korzysta z wersji zgodnie z tagie GIT. Co to daje opisane jest w kolejnym paragrafie.

#### makefile

Dla ułatwienia procesu budowania przygotowany został plik *Makefile*, który pozwala skorzystać z bardzo wygodnej komendy:
```
make jakas_moja_komenda
```

Przygotowany *Makefile* automatyzuje kolejne kroki:
1. Ustala aktualną wersję aplikacji (git tag)
2. Kompresuje statyczną zawartość aplikacji (cały folder `./static`) do archiwum *.tar.gz*
3. Kompiluje aplikację w dwóch wersjach: dla systemu MacOS (darwin) oraz Ubuntu (linux)
4. Wyjściowe pliki zapisuje w folderze `./releases` z odpowiednim dopiskiem w nazwie pliku



## instalacja i uruchomienie

### instalacja od zera

Zakładamy że instalujemy aplikację na systemie linux, np Ubuntu.
Potrzebujemy dostęp do serwera przez *ssh* oraz uprawnienia administratora (do *sudo*)

1. Przygotować pliki w wybranej wersji: archiwum `static_gotech_v0.x.tar.gz`, plik binarn w odpowiedniej wersji i dla odpowiedniego systemu: `gotech_linux_v0.xx`

2. Obydwa pliki skopiować na docelową maszynę, np: 
`scp *v0.xx* host.com:~/gotech/`

3. Na docelowej maszynie (zakładając że to linux) tworzymy użytkownika bez możliwości logowania:
```
sudo useradd -r -s /bin/false gotech
```

4. Kopiujemy plik wykonawczy do wybranej lokalizacji i najlepiej nazywamy go w prosty sposób:
```
sudo cp gotech_linux_v0.x /srv/gotech/gotech
```

5. Rozpakowujemy pliki statyczne do wybranej lokalizacji (domyślnie ten sam folder):
```
sudo tar -xzvf static_gotech.tar.gz -C /srv/gotech/
```

6. Pozwalamy naszej aplikacji słuchać na niskim porcie (domyślny port dla http to 80):
```
sudo setcap CAP_NET_BIND_SERVICE=+eip /path/to/binary
```

7. Tworzymy usługę (linux), w `/etc/systemd/system` tworzymy plik np `gotech.service`:
```
[Unit]
Description=Aplikacja TECH passage.cl
After=network.target
StartLimitIntervalSec=0

[Service]
Type=simple
Restart=always
RestartSec=5
User=gotech
WorkingDirectory=/srv/gotech
ExecStart=/srv/gotech/gotech -config /srv/gotech/config.json

[Install]
WantedBy=multi-user.target
```

Zwracamy uwagę na definicje *User* oraz *ExecStart*

Linia *WorkingDirectory* bardzo istotna - wskazuje na ścieżkę odniesienia, ważne dla relatywnie określonych ścieżek (był problem)

8. Na koniec włączamy usługę
```
sudo systemctl enable gotech
```

### update

Gdy chcemy zaktualizować instalację na systemie linux:

1. Zatrzymujemy usługę:
```
sudo systemctl stop gotech
```

2. Dla pewności, że nie zostawimy niepotrzebnych plików, usuwamy plik wykonawczy i pliki statyczne:
```
sudo rm /srv/gotech/gotech
sudo rm -R /srv/gotech/static

```

3. Powtarzamy kroki (jak przy nowej instalacji): 1), 2), 4), 5), 6)

4. Upewniamy się, że użytkownik aplikacji ma dostęp do plików:
```
sudo chown -R gotech:gotech /srv/gotech
```

5. Uruchamiamy ręcznie aplikację w celu przeprowadzenia migracji bazy (chyba że mamy pewność, że nie jest wymagana):
```
sudo -u gotech ./gotech -migrate=t
```

6. Uruchamiamy usługę:
```
sudo systemctl start gotech
```

### uruchomienie

Gdy wszystko skonfigurowaliśmy zgodnie z instrukcją i nie napotkaliśmy problemów, usługę możemy uruchamiać, wyłączać, restartować i sprawdzać jej status komendą `systemctl`


## TODO

- log to pliku!

