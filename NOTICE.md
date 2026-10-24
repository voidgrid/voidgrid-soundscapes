# Acknowledgements

voidgrid-soundscapes was inspired by [Amniotic](https://github.com/fmtr/amniotic) by Frontmatter AI (fmtr): mixing several ambient recordings into a named mix, serving it as an HTTP audio stream, and driving it from Home Assistant over MQTT. That idea is theirs, and this project exists because of it.

This is an independent rewrite. It was started as a fork, then restarted from scratch in a different language. **No Amniotic code is included.** Amniotic is licensed under Apache-2.0; this project is MIT (see `LICENSE`). If you want the original, supported project, use Amniotic.

Runtime dependencies keep their own licences: [FFmpeg](https://ffmpeg.org/) is invoked as a separate program for decoding and encoding (its licence depends on how your distribution builds it).
