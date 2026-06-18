# Run lakeFS on AWS EC2 with real S3 (IAM role, no static keys)

End-to-end guide to get **lakeFS** running on an **EC2** instance backed by a real **S3**
bucket, authenticating to S3 via the instance's **IAM role** (no access keys anywhere).

> Principle: run lakeFS **in the same AWS account and region as the S3 bucket**, and let it
> reach S3 through an attached IAM role. This avoids cross-account/"god" credentials and
> avoids long-lived secrets entirely.

Everything below is copy-paste; replace the `ALL_CAPS` placeholders.

---

## 1. Create the S3 bucket

```bash
export AWS_REGION=us-east-1
export BUCKET=YOUR_BUCKET_NAME      # globally unique

aws s3api create-bucket --bucket "$BUCKET" --region "$AWS_REGION" \
  $( [ "$AWS_REGION" = "us-east-1" ] || echo --create-bucket-configuration LocationConstraint="$AWS_REGION" )
```

## 2. IAM role (instance profile) for the EC2

Create a role the EC2 will assume, with **least-privilege access to just this bucket**.

Trust policy (`trust.json`):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Principal": { "Service": "ec2.amazonaws.com" }, "Action": "sts:AssumeRole" }
  ]
}
```

Permissions policy (`lakefs-s3.json`) — replace `YOUR_BUCKET_NAME` (twice):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "lakeFSBucket",
      "Effect": "Allow",
      "Action": ["s3:ListBucket", "s3:GetBucketLocation"],
      "Resource": "arn:aws:s3:::YOUR_BUCKET_NAME"
    },
    {
      "Sid": "lakeFSObjects",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject", "s3:PutObject", "s3:DeleteObject",
        "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"
      ],
      "Resource": "arn:aws:s3:::YOUR_BUCKET_NAME/*"
    }
  ]
}
```

```bash
aws iam create-role --role-name lakefs-ec2 \
  --assume-role-policy-document file://trust.json
aws iam put-role-policy --role-name lakefs-ec2 \
  --policy-name lakefs-s3 --policy-document file://lakefs-s3.json
aws iam create-instance-profile --instance-profile-name lakefs-ec2
aws iam add-role-to-instance-profile \
  --instance-profile-name lakefs-ec2 --role-name lakefs-ec2
```

## 3. Launch the EC2 instance

- AMI: Amazon Linux 2023 (or Ubuntu). Size: `t3.small`/`t3.medium` is plenty.
- **Same region** as the bucket.
- **Attach the `lakefs-ec2` instance profile** (IAM role) created above.
- Security group: allow inbound **8000/tcp** from where you'll access it (lock this down;
  ideally only your IP or a load balancer / reverse proxy). SSH 22 as needed.

Attach the profile to an already-running instance if needed:

```bash
aws ec2 associate-iam-instance-profile \
  --instance-id i-XXXXXXXX \
  --iam-instance-profile Name=lakefs-ec2
```

> **Critical gotcha — IMDS hop limit.** lakeFS runs in a Docker container and fetches the
> instance-role credentials from IMDSv2. A container is one extra network hop, but EC2's
> default `HttpPutResponseHopLimit` is **1**, so the token request fails and lakeFS can't
> get credentials (S3 calls then fail with auth errors). Set the hop limit to **2**:
>
> ```bash
> aws ec2 modify-instance-metadata-options --instance-id i-XXXXXXXX \
>   --http-tokens required --http-put-response-hop-limit 2
> ```
>
> (Alternatively run the lakeFS container with `network_mode: host`.)

## 4. Install Docker on the instance

Amazon Linux 2023:

```bash
sudo dnf install -y docker
sudo systemctl enable --now docker
sudo usermod -aG docker ec2-user   # re-login for this to take effect
# docker compose v2 plugin:
sudo mkdir -p /usr/libexec/docker/cli-plugins
sudo curl -sSL "https://github.com/docker/compose/releases/latest/download/docker-compose-linux-x86_64" \
  -o /usr/libexec/docker/cli-plugins/docker-compose
sudo chmod +x /usr/libexec/docker/cli-plugins/docker-compose
```

## 5. Deploy lakeFS

```bash
# get these two files from this repo's examples/ dir
#   docker-compose.aws.yml , .env.aws.example
cp .env.aws.example .env
# edit .env: set AWS_REGION (bucket region) and LAKEFS_ENCRYPT_SECRET ($(openssl rand -hex 24))

docker compose -f docker-compose.aws.yml up -d
docker compose -f docker-compose.aws.yml logs --no-log-prefix lakefs | tail -20
```

You should see `lakeFS ... Up and running`.

## 6. Bootstrap the admin

```bash
curl -s -X POST http://localhost:8000/api/v1/setup_lakefs \
  -H 'Content-Type: application/json' -d '{"username":"admin"}'
```

Returns `access_key_id` + `secret_access_key` — **save them**. Use them for the UI login,
`lakectl`, and the S3 gateway.

## 7. Verify S3 wiring (create a repo + write an object)

```bash
AK=AKIA...    # from step 6
SK=...        # from step 6
BASE=http://localhost:8000/api/v1

# create a repo whose storage namespace is a prefix in your bucket
curl -s -u "$AK:$SK" -X POST "$BASE/repositories" -H 'Content-Type: application/json' \
  -d "{\"name\":\"demo\",\"storage_namespace\":\"s3://YOUR_BUCKET_NAME/demo\",\"default_branch\":\"main\"}"
```

If this returns the repo JSON (not a 4xx/5xx with an S3 error), the IAM role + bucket are
wired correctly — lakeFS just wrote a `demo/_lakefs/dummy` marker into your bucket. Confirm:

```bash
aws s3 ls "s3://YOUR_BUCKET_NAME/demo/_lakefs/"
```

Open the UI at `http://EC2_PUBLIC_IP:8000` and log in with the admin keys.

---

## Production notes

- **TLS / domain**: put lakeFS behind a reverse proxy (ALB + ACM cert, or nginx/Caddy/Traefik)
  and use HTTPS. Don't expose `:8000` raw to the internet long-term.
- **Metadata durability**: the embedded local KV lives on the `lakefs-metadata` Docker volume —
  back it up. For HA / managed durability, switch to **DynamoDB**:
  ```yaml
  LAKEFS_DATABASE_TYPE: dynamodb
  LAKEFS_DATABASE_DYNAMODB_TABLE_NAME: lakefs-kv
  LAKEFS_DATABASE_DYNAMODB_AWS_REGION: ${AWS_REGION}
  # add dynamodb:* on the table to the IAM role; lakeFS auto-creates the table
  ```
- **SSO / multi-user / SCIM**: this minimal setup is single-admin (basic auth). To add OIDC
  login, per-repo groups, and IdP-driven deprovisioning, deploy the full stack in
  [`docker-compose.yml`](docker-compose.yml) (lakeFS + ACL server + this shim) and set
  `OIDC_REDIRECT_URL` to your real `https://lakefs.<domain>/oidc/callback`.
