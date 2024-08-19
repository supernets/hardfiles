# HARDFILES
In today's digital landscape, the majority of image and file-sharing platforms are overburdened with bloatware, inundated with trackers, and riddled with restrictive usage limits. Moreover, they often cram unnecessary features, leaving users longing for a straightforward and secure file-sharing experience...

We designed HardFiles with a singular vision: to simplify and secure the process of file sharing. No fluff, no unnecessary features — just a streamlined, user-centric platform. What's more, we believe in transparency and community involvement, which is why HardFiles is open-source. Explore our service and contribute to its development at [https://hardfiles.org](https://hardfiles.org) now!

🚫 **No JavaScript required to upload files!** 🚫

🛑 **No logs 📜, no tracking 👣, & no analytics!** 📊🚫

🚷 **No weird anime girls or cringe weeb stuff on the homepage** 📵🚫

🔒 **All uploads are shredded securely ✂️🔥 after 24 hours** ⏳🗑️

## Terms of Service
This platform serves as a public file hosting service. It is not actively monitored or overseen for specific content. Users are solely responsible for the content they upload and share. The administrator and owner of this server explicitly disclaim any responsibility for the content hosted and shared by users. Furthermore, the administrator is not liable for any damages, losses, or repercussions, either direct or indirect, resulting from the use of this service or the content found therein. Users are urged to use this service responsibly and ethically.

HardFiles is built on the principle of flexibility. If you choose to run your own instance of our service, you have the autonomy to define your own set of rules tailored to your community or organizational needs. However, when using our official service at [hardfiles.org](https://hardfiles.org), we maintain a minimalistic approach to rules. Our singular, non-negotiable rule is a strict prohibition against child pornography. We are committed to creating a safe environment for all users, and we have zero tolerance for any content that exploits the vulnerable.

## Deployment Guide for HardFiles

### 1. Clone this repository

This is necessary even when using the Docker image as the image does not contain the HardFiles frontend.

```shell
git clone https://git.supernets.org/supernets/hardfiles.git
```

### 2. Configuration:
Start by adjusting the necessary configuration variables in `config.toml`.

### 3. Build and Run 

#### Bare Metal:

Execute the following commands to build and initiate HardFiles:
```shell
go build -o hardfiles main.go
./hardfiles
```

#### Docker Compose:

Execute the following commands to build and initiate HardFiles in Docker:
```shell
docker compose up -d
```

### 3. Web Server Configuration:

By default, HardFiles listens on port `5000`. For production environments, it's recommended to use a robust web server like Nginx or Caddy to proxy traffic to this port.

For obtaining the Let's Encrypt certificates, you can use tools like `certbot` that automatically handle the certification process for you. If you elect to use Caddy, in most circumstances it is able to handle certificates for you using Let's Encrypt.

Remember, by using a reverse proxy, you can run HardFiles without needing root privileges and maintain a more secure environment.

#### Using Nginx as a Reverse Proxy:

A reverse proxy takes requests from the Internet and forwards them to servers in an internal network. By doing so, it ensures that the actual application (in this case, HardFiles) doesn't need to run with root privileges or directly face the Internet, which is a security best practice.

Here's a basic setup for Nginx:

```nginx
server {
    listen 80;
    server_name your_domain.com;

    location / {
        proxy_pass http://localhost:5000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    listen 443 ssl;
    ssl_certificate /etc/letsencrypt/live/your_domain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/your_domain.com/privkey.pem;
}
```

Replace `your_domain.com` with your actual domain name. Save this configuration to a file, say `hardfiles.conf`, inside the `/etc/nginx/sites-available/` directory, and then create a symbolic link to `/etc/nginx/sites-enabled/`. Restart Nginx after this setup.

#### Using Caddy as a Reverse Proxy:

Append the following to the Caddyfile, replacing your_domain.com with your chosen domain.

```caddy
your_domain.com {
    reverse_proxy localhost:5000
}
```

## cURL Uploads

You can upload files using cURL like so:

```shell
curl -F file=@$1 https://hardfiles.org/
```

Additionally, you can specify the amount of time before your upload is removed from the server. Currently the file expiry time must be provided in seconds and is limited to 5 days maximum. The following example will return a file that expires in 48 hours rather than the default of 24 hours.

```shell
curl -F file=@$1 -F expiry=172800 https://hardfiles.org/
```

### Bash Alias

If you frequently upload files to HardFiles via the command line, you can streamline the process by setting up a bash alias. This allows you to use a simple command, like `upload`, to push your files to HardFiles using `curl`.

#### Setting Up:

1. **Edit your `.bashrc` file:** Open your `~/.bashrc` file in a text editor. You can use `nano` or `vim` for this purpose:
```shell
nano ~/.bashrc
```

2. **Add the `upload` function:** At the end of the `.bashrc` file, append the following function (replace the domain if you are running your own instance):
```shell
upload() {
    curl -F file=@$1 https://hardfiles.org/
}
```

3. Reload your .bashrc file: To make the new function available in your current session, reload your .bashrc:
```shell
source ~/.bashrc
```

#### Usage:
Now, you can easily upload files to HardFiles using the upload command followed by the path to your file. For example:

```shell
upload /path/to/your/file.jpg
```

This will upload the specified file to HardFiles and return a direct link to the file.

## Roadmap
- Idea - Uploads stored on a remotely mounted drive or S3 compatible volume, isolating them from the actual service server. Multiple mirrored instances behind a round robin reading from the same remote mount for scaling.
- Random wallpapers as an optional extra, kept simple without javascript. Maybe a local shell script that modifies the index.html on a timer.
- Fix index wallpaper alignment on smartphones.
- Clean up CSS.
- Warrant Canary
- Footer or some link to SupernNETs & this repository & terms of service txt.
- Tor & i2p support services *(This can quite possibly be a very bad idea to operate. Maybe a captcha for .onion/.i2p uploads only...)*

## Credits
- 🚀 **delorean**, our Senior Director of IRC Diplomacy & SuperNets Brand Strategy 🌐 for developing hardfiles.
- 🤝 **hgw**, our  Principal Designer of Digital Aquariums & Rare Fish Showcases 🐠 for branding the product.
- 💼 **acidvegas**, our Global Director of IRC Communications 💬 for funding the project 💰.

___

###### Mirrors
[acid.vegas](https://git.acid.vegas/hardfiles) • [GitHub](https://github.com/supernets/hardfiles) • [GitLab](https://gitlab.com/supernets/hardfiles) • [SuperNETs](https://git.supernets.org/supernets/hardfiles)